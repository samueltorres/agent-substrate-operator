package controller

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/streaming/pkg/httpstream"

	"github.com/samueltorres/agent-substrate-operator/internal/upstream"
)

// The bundled PostgreSQL pod and the identities upstream's
// postgressetup.DefaultConfig creates in it, passwordless because each login
// presents a pod certificate.
const (
	postgresPod       = "postgres-0"
	postgresContainer = "postgres"
)

var postgresSetupArgs = []string{
	"--set=substrate_schema=substrate",
	"--set=substrate_owner_role=substrate_owner",
	"--set=substrate_owner_user=substrate_owner_user",
	"--set=substrate_owner_password=",
	"--set=substrate_readwrite_role=substrate_readwrite",
	"--set=substrate_readwrite_user=substrate_readwrite_user",
	"--set=substrate_readwrite_password=",
}

// PodExecutor runs a command in a pod container, feeding it stdin.
type PodExecutor interface {
	Exec(ctx context.Context, namespace, pod, container string, command []string, stdin string) error
}

// postgresSetupCommand is the psql invocation that runs the setup script as
// the local administrator, whose credentials never leave the pod.
func postgresSetupCommand() ([]string, string) {
	command := append([]string{
		"psql", "--no-psqlrc", "--set=ON_ERROR_STOP=1", "--username", "postgres", "--dbname", "atepg",
	}, postgresSetupArgs...)
	return command, "BEGIN;\n" + upstream.PostgresSetupSQL + "\nCOMMIT;\n"
}

// APIServerExecutor execs through the API server, preferring WebSockets and
// falling back to SPDY as kubectl does.
type APIServerExecutor struct {
	Config    *rest.Config
	Clientset kubernetes.Interface
}

// Exec implements PodExecutor.
func (e *APIServerExecutor) Exec(ctx context.Context, namespace, pod, container string, command []string, stdin string) error {
	req := e.Clientset.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(namespace).Name(pod).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   command,
			Stdin:     true,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)
	ws, err := remotecommand.NewWebSocketExecutor(e.Config, "GET", req.URL().String())
	if err != nil {
		return err
	}
	spdy, err := remotecommand.NewSPDYExecutor(e.Config, "POST", req.URL())
	if err != nil {
		return err
	}
	exec, err := remotecommand.NewFallbackExecutor(ws, spdy, httpstream.IsUpgradeFailure)
	if err != nil {
		return err
	}
	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  strings.NewReader(stdin),
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return fmt.Errorf("%w: %s", err, detail)
		}
		return err
	}
	return nil
}
