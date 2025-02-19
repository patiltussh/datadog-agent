// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package api

import (
	"fmt"

	model "github.com/DataDog/agent-payload/v5/process"
	"github.com/DataDog/zstd_0"
	"github.com/gogo/protobuf/proto"

	"github.com/DataDog/datadog-agent/pkg/telemetry"
)

var (
	tlmBytesIn = telemetry.NewCounter("process", "payloads_bytes_in",
		[]string{"type"}, "Count of bytes before encoding payload")
	tlmBytesOut = telemetry.NewCounter("process", "payloads_bytes_out",
		[]string{"type"}, "Count of bytes after encoding payload")
)

// EncodePayload encodes a process message into a payload
func EncodePayload(m model.MessageBody) ([]byte, error) {
	msgType, err := model.DetectMessageType(m)
	if err != nil {
		return nil, fmt.Errorf("unable to detect message type: %s", err)
	}

	typeTag := "type:" + msgType.String()
	tlmBytesIn.Add(float64(m.Size()), typeTag)

	var encoded []byte
	if msgType == model.TypeCollectorProcEvent {
		encoded, err = proto.Marshal(m)
	} else if msgType == model.TypeCollectorManifest {
		encoded, err = encodeManifestPayload(m)
	} else {
		encoded, err = model.EncodeMessage(model.Message{
			Header: model.MessageHeader{
				Version:  model.MessageV3,
				Encoding: model.MessageEncodingZstdPB,
				Type:     msgType,
			}, Body: m})
	}

	tlmBytesOut.Add(float64(len(encoded)), typeTag)

	return encoded, err
}

func encodeManifestPayload(m model.MessageBody) ([]byte, error) {
	pb, err := proto.Marshal(m)
	if err != nil {
		return pb, err
	}

	encoded, err := zstd_0.Compress(nil, pb)
	return encoded, err
}
