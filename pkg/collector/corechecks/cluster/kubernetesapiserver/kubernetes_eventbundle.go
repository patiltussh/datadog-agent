// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2017-present Datadog, Inc.
//go:build kubeapiserver
// +build kubeapiserver

package kubernetesapiserver

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	cache "github.com/patrickmn/go-cache"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/DataDog/datadog-agent/pkg/metrics"
	"github.com/DataDog/datadog-agent/pkg/util/kubernetes"
	as "github.com/DataDog/datadog-agent/pkg/util/kubernetes/apiserver"
	"github.com/DataDog/datadog-agent/pkg/util/log"
)

type kubernetesEventBundle struct {
	objUID        types.UID              // Unique object Identifier used as the Aggregation key
	namespace     string                 // namespace of the bundle
	readableKey   string                 // Formated key used in the Title in the events
	component     string                 // Used to identify the Kubernetes component which generated the event
	events        []*v1.Event            // List of events in the bundle
	name          string                 // name of the bundle
	kind          string                 // kind of the bundle
	timeStamp     float64                // Used for the new events in the bundle to specify when they first occurred
	lastTimestamp float64                // Used for the modified events in the bundle to specify when they last occurred
	countByAction map[string]int         // Map of count per action to aggregate several events from the same ObjUid in one event
	nodename      string                 // Stores the nodename that should be used to submit the events
	alertType     metrics.EventAlertType // The Datadog event type
}

func newKubernetesEventBundler(event *v1.Event) *kubernetesEventBundle {
	return &kubernetesEventBundle{
		objUID:        event.InvolvedObject.UID,
		component:     event.Source.Component,
		countByAction: make(map[string]int),
		alertType:     getDDAlertType(event.Type),
	}
}

func (b *kubernetesEventBundle) addEvent(event *v1.Event) error {
	if event.InvolvedObject.UID != b.objUID {
		return fmt.Errorf("mismatching Object UIDs: %s != %s", event.InvolvedObject.UID, b.objUID)
	}

	b.events = append(b.events, event)
	b.namespace = event.InvolvedObject.Namespace

	// We do not process the events in chronological order necessarily.
	// We only care about the first time they occurred, the last time and the count.
	b.timeStamp = float64(event.FirstTimestamp.Unix())
	b.lastTimestamp = math.Max(b.lastTimestamp, float64(event.LastTimestamp.Unix()))

	b.countByAction[fmt.Sprintf("**%s**: %s\n", event.Reason, event.Message)] += int(event.Count)
	b.kind = event.InvolvedObject.Kind
	b.name = event.InvolvedObject.Name

	if event.InvolvedObject.Kind == "Pod" || event.InvolvedObject.Kind == "Node" {
		b.nodename = event.Source.Host
	}

	if event.InvolvedObject.Namespace == "" {
		b.readableKey = fmt.Sprintf("%s %s", event.InvolvedObject.Kind, event.InvolvedObject.Name)
	} else {
		b.readableKey = fmt.Sprintf("%s %s/%s", event.InvolvedObject.Kind, event.InvolvedObject.Namespace, event.InvolvedObject.Name)
	}

	return nil
}

func (b *kubernetesEventBundle) formatEvents(clusterName string, providerIDCache *cache.Cache) (metrics.Event, error) {
	if len(b.events) == 0 {
		return metrics.Event{}, errors.New("no event to export")
	}

	tags := []string{
		fmt.Sprintf("source_component:%s", b.component),
		fmt.Sprintf("kubernetes_kind:%s", b.kind),
		fmt.Sprintf("name:%s", b.name),
	}

	if kindTag := getKindTag(b.kind, b.name); kindTag != "" {
		tags = append(tags, kindTag)
	}

	hostname := b.nodename
	if b.nodename != "" {
		// Adding the clusterName to the nodename if present
		if clusterName != "" {
			hostname = hostname + "-" + clusterName
		}

		// Find provider ID from cache or find via node spec from APIserver
		hostProviderID, hit := providerIDCache.Get(b.nodename)
		if hit {
			tags = append(tags, fmt.Sprintf("host_provider_id:%s", hostProviderID))
		} else {
			hostProviderID := getHostProviderID(b.nodename)
			if hostProviderID != "" {
				providerIDCache.Set(b.nodename, hostProviderID, cache.NoExpiration)
				tags = append(tags, fmt.Sprintf("host_provider_id:%s", hostProviderID))
			}
		}
	}

	if b.namespace != "" {
		tags = append(tags,
			// TODO remove the deprecated namespace tag, we should
			// only rely on kube_namespace
			fmt.Sprintf("namespace:%s", b.namespace),
			fmt.Sprintf("kube_namespace:%s", b.namespace),
		)
	}

	// If hostname was not defined, the aggregator will then set the local hostname
	output := metrics.Event{
		Title:          fmt.Sprintf("Events from the %s", b.readableKey),
		Priority:       metrics.EventPriorityNormal,
		Host:           hostname,
		SourceTypeName: "kubernetes",
		EventType:      kubernetesAPIServerCheckName,
		Ts:             int64(b.lastTimestamp),
		Tags:           tags,
		AggregationKey: fmt.Sprintf("kubernetes_apiserver:%s", b.objUID),
		AlertType:      b.alertType,
		Text:           b.formatEventText(),
	}

	return output, nil
}

func (b *kubernetesEventBundle) formatEventText() string {
	eventText := fmt.Sprintf(
		"%%%%%% \n%s \n _Events emitted by the %s seen at %s since %s_ \n\n %%%%%%",
		formatStringIntMap(b.countByAction),
		b.component,
		time.Unix(int64(b.lastTimestamp), 0),
		time.Unix(int64(b.timeStamp), 0),
	)

	// Escape the ~ character to not strike out the text
	eventText = strings.ReplaceAll(eventText, "~", "\\~")

	return eventText
}

// getKindTag returns the kube_<kind>:<name> tag.
// The exact same tag names and object kinds are supported by the tagger.
// It returns an empty string if the kind doesn't correspond to a known/supported kind tag.
func getKindTag(kind, name string) string {
	if tagName, found := kubernetes.KindToTagName[kind]; found {
		return fmt.Sprintf("%s:%s", tagName, name)
	}
	return ""
}

func getHostProviderID(nodename string) string {
	cl, err := as.GetAPIClient()
	if err != nil {
		log.Warnf("Can't create client to query the API Server: %v", err)
		return ""
	}

	node, err := as.GetNode(cl, nodename)
	if err != nil {
		log.Warnf("Can't get node from API Server: %v", err)
		return ""
	}

	providerID := node.Spec.ProviderID
	if providerID == "" {
		log.Warnf("ProviderID not found")
		return ""
	}

	// e.g. gce://datadog-test-cluster/us-east1-a/some-instance-id or aws:///us-east-1e/i-instanceid
	s := strings.Split(providerID, "/")
	return s[len(s)-1]
}

func formatStringIntMap(input map[string]int) string {
	var parts []string
	for k, v := range input {
		parts = append(parts, fmt.Sprintf("%d %s", v, k))
	}
	return strings.Join(parts, " ")
}

// getDDAlertType converts kubernetes event types into datadog alert types
func getDDAlertType(k8sType string) metrics.EventAlertType {
	switch k8sType {
	case v1.EventTypeNormal:
		return metrics.EventAlertTypeInfo
	case v1.EventTypeWarning:
		return metrics.EventAlertTypeWarning
	default:
		log.Debugf("Unknown event type '%s'", k8sType)
		return metrics.EventAlertTypeInfo
	}
}
