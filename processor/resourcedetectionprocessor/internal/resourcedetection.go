// Copyright The OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package internal contains an interface for detecting resource information,
// and a provider to merge the resources returned by a slice of custom detectors.
package internal // import "github.com/open-telemetry/opentelemetry-collector-contrib/processor/resourcedetectionprocessor/internal"

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.uber.org/zap"
)

type DetectorType string

type Detector interface {
	Detect(ctx context.Context) (resource pcommon.Resource, schemaURL string, err error)
}

type DetectorConfig interface{}

type ResourceDetectorConfig interface {
	GetConfigFromType(DetectorType) DetectorConfig
}

type DetectorFactory func(component.ProcessorCreateSettings, DetectorConfig) (Detector, error)

type ResourceProviderFactory struct {
	// detectors holds all possible detector types.
	detectors map[DetectorType]DetectorFactory
}

func NewProviderFactory(detectors map[DetectorType]DetectorFactory) *ResourceProviderFactory {
	return &ResourceProviderFactory{detectors: detectors}
}

func (f *ResourceProviderFactory) CreateResourceProvider(
	params component.ProcessorCreateSettings,
	timeout time.Duration,
	attributes []string,
	detectorConfigs ResourceDetectorConfig,
	detectorTypes ...DetectorType) (*ResourceProvider, error) {
	detectors, err := f.getDetectors(params, detectorConfigs, detectorTypes)
	if err != nil {
		return nil, err
	}

	attributesToKeep := make(map[string]struct{})
	if len(attributes) > 0 {
		for _, attribute := range attributes {
			attributesToKeep[attribute] = struct{}{}
		}
	}

	provider := NewResourceProvider(params.Logger, timeout, attributesToKeep, detectors...)
	return provider, nil
}

func (f *ResourceProviderFactory) getDetectors(params component.ProcessorCreateSettings, detectorConfigs ResourceDetectorConfig, detectorTypes []DetectorType) ([]Detector, error) {
	detectors := make([]Detector, 0, len(detectorTypes))
	for _, detectorType := range detectorTypes {
		detectorFactory, ok := f.detectors[detectorType]
		if !ok {
			return nil, fmt.Errorf("invalid detector key: %v", detectorType)
		}

		detector, err := detectorFactory(params, detectorConfigs.GetConfigFromType(detectorType))
		if err != nil {
			return nil, fmt.Errorf("failed creating detector type %q: %w", detectorType, err)
		}

		detectors = append(detectors, detector)
	}

	return detectors, nil
}

type ResourceProvider struct {
	logger           *zap.Logger
	timeout          time.Duration
	detectors        []Detector
	detectedResource *resourceResult
	once             sync.Once
	attributesToKeep map[string]struct{}
}

type resourceResult struct {
	resource  pcommon.Resource
	schemaURL string
	err       error
}

func NewResourceProvider(logger *zap.Logger, timeout time.Duration, attributesToKeep map[string]struct{}, detectors ...Detector) *ResourceProvider {
	return &ResourceProvider{
		logger:           logger,
		timeout:          timeout,
		detectors:        detectors,
		attributesToKeep: attributesToKeep,
	}
}

func (p *ResourceProvider) Get(ctx context.Context, client *http.Client) (resource pcommon.Resource, schemaURL string, err error) {
	p.once.Do(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, client.Timeout)
		defer cancel()
		p.detectResource(ctx)
	})

	return p.detectedResource.resource, p.detectedResource.schemaURL, p.detectedResource.err
}

func (p *ResourceProvider) detectResource(ctx context.Context) {
	p.detectedResource = &resourceResult{}

	res := pcommon.NewResource()
	mergedSchemaURL := ""

	p.logger.Info("began detecting resource information")

	for _, detector := range p.detectors {
		r, schemaURL, err := detector.Detect(ctx)
		if err != nil {
			p.logger.Warn("failed to detect resource", zap.Error(err))
		} else {
			mergedSchemaURL = MergeSchemaURL(mergedSchemaURL, schemaURL)
			MergeResource(res, r, false)
		}
	}

	droppedAttributes := filterAttributes(res.Attributes(), p.attributesToKeep)

	p.logger.Info("detected resource information", zap.Any("resource", AttributesToMap(res.Attributes())))
	if len(droppedAttributes) > 0 {
		p.logger.Info("dropped resource information", zap.Strings("resource keys", droppedAttributes))
	}

	p.detectedResource.resource = res
	p.detectedResource.schemaURL = mergedSchemaURL
}

func AttributesToMap(am pcommon.Map) map[string]interface{} {
	mp := make(map[string]interface{}, am.Len())
	am.Range(func(k string, v pcommon.Value) bool {
		mp[k] = UnwrapAttribute(v)
		return true
	})
	return mp
}

func UnwrapAttribute(v pcommon.Value) interface{} {
	switch v.Type() {
	case pcommon.ValueTypeBool:
		return v.Bool()
	case pcommon.ValueTypeInt:
		return v.Int()
	case pcommon.ValueTypeDouble:
		return v.Double()
	case pcommon.ValueTypeStr:
		return v.Str()
	case pcommon.ValueTypeSlice:
		return getSerializableArray(v.Slice())
	case pcommon.ValueTypeMap:
		return AttributesToMap(v.Map())
	default:
		return nil
	}
}

func getSerializableArray(inArr pcommon.Slice) []interface{} {
	var outArr []interface{}
	for i := 0; i < inArr.Len(); i++ {
		outArr = append(outArr, UnwrapAttribute(inArr.At(i)))
	}

	return outArr
}

func MergeSchemaURL(currentSchemaURL string, newSchemaURL string) string {
	if currentSchemaURL == "" {
		return newSchemaURL
	}
	if newSchemaURL == "" {
		return currentSchemaURL
	}
	if currentSchemaURL == newSchemaURL {
		return currentSchemaURL
	}
	// TODO: handle the case when the schema URLs are different by performing
	// schema conversion. For now we simply ignore the new schema URL.
	return currentSchemaURL
}

func filterAttributes(am pcommon.Map, attributesToKeep map[string]struct{}) []string {
	if len(attributesToKeep) > 0 {
		var droppedAttributes []string
		am.RemoveIf(func(k string, v pcommon.Value) bool {
			_, keep := attributesToKeep[k]
			if !keep {
				droppedAttributes = append(droppedAttributes, k)
			}
			return !keep
		})
		return droppedAttributes
	}
	return nil
}

func MergeResource(to, from pcommon.Resource, overrideTo bool) {
	if IsEmptyResource(from) {
		return
	}

	toAttr := to.Attributes()
	from.Attributes().Range(func(k string, v pcommon.Value) bool {
		if overrideTo {
			v.CopyTo(toAttr.PutEmpty(k))
		} else {
			if _, found := toAttr.Get(k); !found {
				v.CopyTo(toAttr.PutEmpty(k))
			}
		}
		return true
	})
}

func IsEmptyResource(res pcommon.Resource) bool {
	return res.Attributes().Len() == 0
}
