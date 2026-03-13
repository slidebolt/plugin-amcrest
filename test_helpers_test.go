package main

import (
	runner "github.com/slidebolt/sdk-runner"
	"github.com/slidebolt/sdk-types"
)

type testEventSink interface {
	EmitEvent(evt types.InboundEvent) error
}

type testEventService struct {
	sink testEventSink
}

func (l testEventService) PublishEvent(evt types.InboundEvent) error {
	if l.sink == nil {
		return nil
	}
	return l.sink.EmitEvent(evt)
}

func initTestAdapter(p *PluginAdapter, raw deviceRawStore, sink testEventSink) {
	p.rawStore = raw
	_, _ = p.Initialize(runner.PluginContext{
		Events: testEventService{sink: sink},
	})
}
