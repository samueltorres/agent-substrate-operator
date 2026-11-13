package render

// The flag settings below are ported from upstream
// cmd/ate-setup/internal/steps (apienv.go, idpserver.go, overlay.go) at the
// embedded release.

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// egressSPIFFEID is the SPIFFE ID atenet-egress's pod certificate carries.
const egressSPIFFEID = "spiffe://cluster.local/ns/" + Namespace + "/sa/atenet-egress"

// systemFlags sets the flags upstream's installer owns on the system bundle:
// ate-api-server's bootstrap owners and egress gateway IDs, ate-idp-server's
// issuer, and the router's ingress allowlist.
func (r *renderer) systemFlags(objs []*Object) error {
	api, err := find(objs, "Deployment", "ate-api-server")
	if err != nil {
		return err
	}
	if err := setFlag(api, "ate-api-server", "--authz-bootstrap-owners",
		strings.Join(r.spec.APIServer.AuthzBootstrapOwners, ",")); err != nil {
		return err
	}
	// Passing the flag replaces ate-api-server's default, atenet-egress, so
	// it leads the list.
	var gateways []string
	if extra := r.spec.APIServer.EgressGatewayExtraSPIFFEIDs; len(extra) > 0 {
		gateways = []string{egressSPIFFEID}
		for _, id := range extra {
			if !slices.Contains(gateways, id) {
				gateways = append(gateways, id)
			}
		}
	}
	if err := setFlag(api, "ate-api-server", "--egress-gateway-spiffe-id", gateways...); err != nil {
		return err
	}

	idp, err := find(objs, "Deployment", "ate-idp-server")
	if err != nil {
		return err
	}
	if err := setFlag(idp, "ate-idp-server", "--issuer", r.spec.ActorJWT.Issuer); err != nil {
		return err
	}

	// The router serves no traffic without an allowlist.
	if ids := r.spec.Atenet.IngressAllowedSPIFFEIDs; len(ids) > 0 {
		router, err := find(objs, "Deployment", "atenet-router")
		if err != nil {
			return err
		}
		if err := setFlag(router, "atenet-router", "--ingress-allowed-spiffe-ids", strings.Join(ids, ",")); err != nil {
			return err
		}
	}
	return nil
}

// setFlag replaces every flag arg on the named container with one
// flag=value arg per value, in order. No values drops the flag.
func setFlag(obj *Object, container, flag string, values ...string) error {
	return mutateContainer(obj, "containers", container, func(c map[string]any) error {
		args, _ := c["args"].([]any)
		args = slices.DeleteFunc(slices.Clone(args), func(a any) bool {
			s, _ := a.(string)
			return s == flag || strings.HasPrefix(s, flag+"=")
		})
		for _, v := range values {
			args = append(args, flag+"="+v)
		}
		c["args"] = args
		return nil
	})
}

var snapshotLocationSegment = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// validateSnapshotLocation is upstream's resources.ValidateSnapshotLocation:
// a bucket and a path whose segments appending names to cannot redirect.
func validateSnapshotLocation(location string) error {
	if location == "" {
		return fmt.Errorf("storage.snapshotLocation is required")
	}
	u, err := url.Parse(location)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return fmt.Errorf("storage.snapshotLocation %q is not SCHEME://BUCKET[/PREFIX/]", location)
	}
	path := strings.TrimSuffix(strings.TrimPrefix(u.EscapedPath(), "/"), "/")
	if path == "" {
		return nil
	}
	for segment := range strings.SplitSeq(path, "/") {
		if !snapshotLocationSegment.MatchString(segment) {
			return fmt.Errorf("storage.snapshotLocation %q: path segment %q must be ASCII letters, digits, '_' and '-'", location, segment)
		}
		if strings.EqualFold(segment, "atespaces") {
			return fmt.Errorf("storage.snapshotLocation %q must not contain an atespaces segment; snapshots are stored under <location>/atespaces/", location)
		}
	}
	return nil
}
