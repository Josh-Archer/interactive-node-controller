package reporter

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	v1alpha1 "github.com/Josh-Archer/interactive-node-controller/api/v1alpha1"
	"github.com/Josh-Archer/interactive-node-controller/internal/config"
)

type KubernetesReporter struct {
	client    *http.Client
	endpoint  string
	tokenFile string
}

func NewKubernetes(cfg config.KubernetesConfig) (*KubernetesReporter, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	caData, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read Kubernetes CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caData) {
		return nil, fmt.Errorf("Kubernetes CA file contains no certificates")
	}
	client := &http.Client{
		Timeout: cfg.Timeout.Duration,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    roots,
		}},
	}
	return NewKubernetesWithClient(cfg, client)
}

// NewKubernetesWithClient permits deterministic transport testing. Validation
// still fixes the request to one namespaced NodeActivity status endpoint.
func NewKubernetesWithClient(cfg config.KubernetesConfig, client *http.Client) (*KubernetesReporter, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if client == nil {
		return nil, fmt.Errorf("HTTP client is required")
	}
	base, _ := url.Parse(cfg.APIServer)
	base.Path = fmt.Sprintf("/apis/%s/%s/namespaces/%s/%s/%s/status", v1alpha1.Group, v1alpha1.Version, cfg.Namespace, v1alpha1.Resource, cfg.Name)
	return &KubernetesReporter{client: client, endpoint: base.String(), tokenFile: cfg.TokenFile}, nil
}

func (r *KubernetesReporter) Report(ctx context.Context, status Status) error {
	token, err := os.ReadFile(r.tokenFile)
	if err != nil {
		return fmt.Errorf("read reporter token: %w", err)
	}
	trimmedToken := strings.TrimSpace(string(token))
	if trimmedToken == "" {
		return fmt.Errorf("reporter token is empty")
	}
	payload, err := json.Marshal(struct {
		Status Status `json:"status"`
	}{Status: status})
	if err != nil {
		return fmt.Errorf("encode NodeActivity status patch: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPatch, r.endpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("create NodeActivity request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+trimmedToken)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/merge-patch+json")
	request.Header.Set("User-Agent", "interactive-node-controller-host-reporter/phase1")

	response, err := r.client.Do(request)
	if err != nil {
		return fmt.Errorf("patch NodeActivity status: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("patch NodeActivity status returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

var _ Reporter = (*KubernetesReporter)(nil)
