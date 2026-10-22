// Copyright 2026 Google LLC
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

package render

// The marker splices below are ported from upstream
// cmd/ate-setup/internal/steps/overlay.go at the embedded release.

import (
	"fmt"
	"strings"
)

// credentialProvider is the provider the egress gateway asks for credentials.
// The zero value turns injection off.
type credentialProvider struct {
	name    string
	address string
}

func (p credentialProvider) enabled() bool { return p.name != "" }

// serverName is the SAN the gateway expects on the provider's serving
// certificate: the address without its port.
func (p credentialProvider) serverName() string {
	if i := strings.LastIndex(p.address, ":"); i >= 0 {
		return p.address[:i]
	}
	return p.address
}

// patchEgress splices the additional ext_proc filter, when extproc names one,
// and the credential provider flags into the Envoy egress manifest.
func patchEgress(raw []byte, extproc string, provider credentialProvider) ([]byte, error) {
	if extproc != "" {
		var err error
		if raw, err = spliceExtproc(raw, extproc); err != nil {
			return nil, err
		}
	}
	var flags string
	if provider.enabled() {
		flags = fmt.Sprintf(`- --credential-provider-name=%s
- --credential-provider-address=%s
- --credential-provider-ca-file=/run/servicedns.podcert.ate.dev/trust-bundle.pem
- --credential-provider-client-cert=/run/podidentity.podcert.ate.dev/credential-bundle.pem
- --credential-provider-server-name=%s`, provider.name, provider.address, provider.serverName())
	}
	return replaceMarker(raw, "#ATE_EGRESS_INJECT_FLAGS", flags, 1)
}

// replaceMarker replaces each line starting with marker by block, indented
// like the marker, and fails unless it finds want of them.
func replaceMarker(raw []byte, marker, block string, want int) ([]byte, error) {
	var out []string
	replaced := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), marker) {
			out = append(out, line)
			continue
		}
		indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		if block != "" {
			for _, l := range strings.Split(block, "\n") {
				if l == "" {
					out = append(out, "")
				} else {
					out = append(out, indent+l)
				}
			}
		}
		replaced++
	}
	if replaced != want {
		return nil, fmt.Errorf("expected %d %s markers, found %d", want, marker, replaced)
	}
	return []byte(strings.Join(out, "\n")), nil
}

// spliceExtproc adds the external processor authorization filter to both
// egress listeners and its cluster to the cluster list. spec is
// NAMESPACE/SERVICE:PORT.
func spliceExtproc(raw []byte, spec string) ([]byte, error) {
	namespace, svcPort, ok := strings.Cut(spec, "/")
	service, port, ok2 := strings.Cut(svcPort, ":")
	if !ok || !ok2 {
		return nil, fmt.Errorf("additionalEgressExtprocService %q is not NAMESPACE/SERVICE:PORT", spec)
	}
	address := fmt.Sprintf("%s.%s.svc.cluster.local", service, namespace)
	serverName := fmt.Sprintf("%s.%s.svc", service, namespace)

	raw, err := replaceMarker(raw, "#ATE_MITM_EXTPROC_FILTER", extprocFilter, 2)
	if err != nil {
		return nil, err
	}
	return replaceMarker(raw, "#ATE_MITM_EXTPROC_CLUSTER", fmt.Sprintf(extprocCluster,
		serverName, serverName, address, port), 1)
}

const extprocFilter = `- name: envoy.filters.http.ext_proc
  typed_config:
    "@type": type.googleapis.com/envoy.extensions.filters.http.ext_proc.v3.ExternalProcessor
    grpc_service:
      envoy_grpc:
        cluster_name: additional_egress_ext_proc
      timeout: 2s
    failure_mode_allow: false
    message_timeout: 2s
    request_attributes:
    - filter_state['dev.ate.actor.identity']
    processing_mode:
      request_header_mode: SEND
      response_header_mode: SKIP
      request_body_mode: NONE
      response_body_mode: NONE
      request_trailer_mode: SKIP
      response_trailer_mode: SKIP
    mutation_rules:
      disallow_system: true
      disallow_is_error: true`

// extprocCluster takes the SNI, the SAN to match, the address, and the port.
const extprocCluster = `- name: additional_egress_ext_proc
  type: STRICT_DNS
  lb_policy: ROUND_ROBIN
  connect_timeout: 1s
  typed_extension_protocol_options:
    envoy.extensions.upstreams.http.v3.HttpProtocolOptions:
      "@type": type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions
      explicit_http_config:
        http2_protocol_options: {}
  transport_socket:
    name: envoy.transport_sockets.tls
    typed_config:
      "@type": type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.UpstreamTlsContext
      sni: %s
      common_tls_context:
        tls_params:
          tls_minimum_protocol_version: TLSv1_3
          tls_maximum_protocol_version: TLSv1_3
        tls_certificate_sds_secret_configs:
        - name: podidentity_client_cert
          sds_config:
            resource_api_version: V3
            path_config_source:
              path: /etc/envoy/sds-podidentity-cert.yaml
        combined_validation_context:
          default_validation_context:
            match_typed_subject_alt_names:
            - san_type: DNS
              matcher:
                exact: %s
          validation_context_sds_secret_config:
            name: servicedns_validation_context
            sds_config:
              resource_api_version: V3
              path_config_source:
                path: /etc/envoy/sds-servicedns-validation.yaml
  load_assignment:
    cluster_name: additional_egress_ext_proc
    endpoints:
    - lb_endpoints:
      - endpoint:
          address:
            socket_address:
              address: %s
              port_value: %s`
