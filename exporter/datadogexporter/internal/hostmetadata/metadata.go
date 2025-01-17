// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

// Package metadata is responsible for collecting host metadata from different providers
// such as EC2, ECS, AWS, etc and pushing it to Datadog.
package hostmetadata // import "github.com/open-telemetry/opentelemetry-collector-contrib/exporter/datadogexporter/internal/hostmetadata"

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/DataDog/opentelemetry-mapping-go/pkg/inframetadata/payload"
	"github.com/DataDog/opentelemetry-mapping-go/pkg/otlp/attributes"
	ec2Attributes "github.com/DataDog/opentelemetry-mapping-go/pkg/otlp/attributes/ec2"
	"github.com/DataDog/opentelemetry-mapping-go/pkg/otlp/attributes/gcp"
	"github.com/DataDog/opentelemetry-mapping-go/pkg/otlp/attributes/source"
	"go.opentelemetry.io/collector/exporter"
	"go.opentelemetry.io/collector/pdata/pcommon"
	conventions "go.opentelemetry.io/collector/semconv/v1.6.1"
	"go.uber.org/zap"

	"github.com/open-telemetry/opentelemetry-collector-contrib/exporter/datadogexporter/internal/clientutil"
	"github.com/open-telemetry/opentelemetry-collector-contrib/exporter/datadogexporter/internal/hostmetadata/internal/ec2"
	"github.com/open-telemetry/opentelemetry-collector-contrib/exporter/datadogexporter/internal/hostmetadata/internal/gohai"
	"github.com/open-telemetry/opentelemetry-collector-contrib/exporter/datadogexporter/internal/hostmetadata/internal/system"
	"github.com/open-telemetry/opentelemetry-collector-contrib/exporter/datadogexporter/internal/scrub"
)

// metadataFromAttributes gets metadata info from attributes following
// OpenTelemetry semantic conventions
func metadataFromAttributes(attrs pcommon.Map) *payload.HostMetadata {
	hm := &payload.HostMetadata{Meta: &payload.Meta{}, Tags: &payload.HostTags{}}

	if src, ok := attributes.SourceFromAttrs(attrs); ok && src.Kind == source.HostnameKind {
		hm.InternalHostname = src.Identifier
		hm.Meta.Hostname = src.Identifier
	}

	// AWS EC2 resource metadata
	cloudProvider, ok := attrs.Get(conventions.AttributeCloudProvider)
	switch {
	case ok && cloudProvider.Str() == conventions.AttributeCloudProviderAWS:
		ec2HostInfo := ec2Attributes.HostInfoFromAttributes(attrs)
		hm.Meta.InstanceID = ec2HostInfo.InstanceID
		hm.Meta.EC2Hostname = ec2HostInfo.EC2Hostname
		hm.Tags.OTel = append(hm.Tags.OTel, ec2HostInfo.EC2Tags...)
	case ok && cloudProvider.Str() == conventions.AttributeCloudProviderGCP:
		gcpHostInfo := gcp.HostInfoFromAttrs(attrs)
		hm.Tags.GCP = gcpHostInfo.GCPTags
		hm.Meta.HostAliases = append(hm.Meta.HostAliases, gcpHostInfo.HostAliases...)
	}

	return hm
}

func fillHostMetadata(params exporter.CreateSettings, pcfg PusherConfig, p source.Provider, hm *payload.HostMetadata) {
	// Could not get hostname from attributes
	if hm.InternalHostname == "" {
		if src, err := p.Source(context.TODO()); err == nil && src.Kind == source.HostnameKind {
			hm.InternalHostname = src.Identifier
			hm.Meta.Hostname = src.Identifier
		}
	}

	// This information always gets filled in here
	// since it does not come from OTEL conventions
	hm.Flavor = params.BuildInfo.Command
	hm.Version = params.BuildInfo.Version
	hm.Tags.OTel = append(hm.Tags.OTel, pcfg.ConfigTags...)
	hm.Payload = gohai.NewPayload(params.Logger)
	hm.Processes = gohai.NewProcessesPayload(hm.Meta.Hostname, params.Logger)
	// EC2 data was not set from attributes
	if hm.Meta.EC2Hostname == "" {
		ec2HostInfo := ec2.GetHostInfo(params.Logger)
		hm.Meta.EC2Hostname = ec2HostInfo.EC2Hostname
		hm.Meta.InstanceID = ec2HostInfo.InstanceID
	}

	// System data was not set from attributes
	if hm.Meta.SocketHostname == "" {
		systemHostInfo := system.GetHostInfo(params.Logger)
		hm.Meta.SocketHostname = systemHostInfo.OS
		hm.Meta.SocketFqdn = systemHostInfo.FQDN
	}
}

func pushMetadata(pcfg PusherConfig, params exporter.CreateSettings, metadata *payload.HostMetadata) error {
	if metadata.Meta.Hostname == "" {
		// if the hostname is empty, don't send metadata; we don't need it.
		params.Logger.Debug("Skipping host metadata since the hostname is empty")
		return nil
	}

	path := pcfg.MetricsEndpoint + "/intake"
	buf, _ := json.Marshal(metadata)
	req, _ := http.NewRequest(http.MethodPost, path, bytes.NewBuffer(buf))
	clientutil.SetDDHeaders(req.Header, params.BuildInfo, pcfg.APIKey)
	clientutil.SetExtraHeaders(req.Header, clientutil.JSONHeaders)
	client := clientutil.NewHTTPClient(pcfg.TimeoutSettings, pcfg.InsecureSkipVerify)
	resp, err := client.Do(req)

	if err != nil {
		return err
	}

	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf(
			"'%s' error when sending metadata payload to %s",
			resp.Status,
			path,
		)
	}

	return nil
}

func pushMetadataWithRetry(retrier *clientutil.Retrier, params exporter.CreateSettings, pcfg PusherConfig, hostMetadata *payload.HostMetadata) {
	params.Logger.Debug("Sending host metadata payload", zap.Any("payload", hostMetadata))

	_, err := retrier.DoWithRetries(context.Background(), func(context.Context) error {
		return pushMetadata(pcfg, params, hostMetadata)
	})

	if err != nil {
		params.Logger.Warn("Sending host metadata failed", zap.Error(err))
	} else {
		params.Logger.Info("Sent host metadata")
	}

}

// Pusher pushes host metadata payloads periodically to Datadog intake
func Pusher(ctx context.Context, params exporter.CreateSettings, pcfg PusherConfig, p source.Provider, attrs pcommon.Map) {
	// Push metadata every 30 minutes
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()
	defer params.Logger.Debug("Shut down host metadata routine")
	retrier := clientutil.NewRetrier(params.Logger, pcfg.RetrySettings, scrub.NewScrubber())

	// Get host metadata from resources and fill missing info using our exporter.
	// Currently we only retrieve it once but still send the same payload
	// every 30 minutes for consistency with the Datadog Agent behavior.
	//
	// All fields that are being filled in by our exporter
	// do not change over time. If this ever changes `hostMetadata`
	// *must* be deep copied before calling `fillHostMetadata`.
	hostMetadata := &payload.HostMetadata{Meta: &payload.Meta{}, Tags: &payload.HostTags{}}
	if pcfg.UseResourceMetadata {
		hostMetadata = metadataFromAttributes(attrs)
	}
	fillHostMetadata(params, pcfg, p, hostMetadata)

	// Run one first time at startup
	pushMetadataWithRetry(retrier, params, pcfg, hostMetadata)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C: // Send host metadata
			pushMetadataWithRetry(retrier, params, pcfg, hostMetadata)
		}
	}
}
