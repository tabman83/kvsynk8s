//go:build integration

// Copyright kvsynk8s contributors.
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azsecrets"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// T030: validates keyVaultReader (keyvault.go) against a real Lowkey Vault
// container -- the real azsecrets wire protocol (TLS, the Key Vault
// challenge-auth handshake, the actual REST payload shapes), not a fake.
//
// Lowkey Vault (github.com/nagyesta/lowkey-vault) checks that an
// Authorization header is present on the real Key Vault operations but does
// not validate its contents (community docs: "will check whether
// [credentials] are there but ignore the value"), so any non-empty bearer
// token satisfies it -- see stubCredential below. This was confirmed
// directly against the container in this environment before writing this
// suite: an anonymous GET gets the standard Key Vault 401 + WWW-Authenticate
// challenge, and a GET with any bearer token succeeds, exactly the shape
// azsecrets' own challenge-auth policy expects. That confirms the research.
// md R10 fallback (a hand-rolled HTTPS stub) is not needed here: the real
// azsecrets client, driven by the real challenge-auth flow, works against
// this emulator unmodified.
//
// vaultURL(vaultName) in keyvault.go is hardcoded to the public Azure cloud
// (https://<name>.vault.azure.net/), so these tests build a keyVaultReader
// directly (same package: access to its unexported fields) and pre-seed its
// client cache with a client pointed at the container instead, bypassing
// that production URL construction -- the same GetLatest code path under
// test either way.
const testVaultName = "integration-test-vault"

// stubCredential is an azcore.TokenCredential that always returns a fixed,
// non-empty token. Lowkey Vault only checks a token is present, never its
// contents (see package doc comment above), so this is sufficient to drive
// azsecrets' real challenge-auth flow end to end without needing a real
// Microsoft Entra tenant.
type stubCredential struct{}

func (stubCredential) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: "integration-test-token", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

// startLowkeyVault launches a Lowkey Vault container and returns a
// keyVaultReader whose client cache is pre-seeded for testVaultName, plus a
// raw *azsecrets.Client for out-of-band setup (creating/disabling secrets)
// the SecretReader interface doesn't expose.
func startLowkeyVault(t *testing.T) (*keyVaultReader, *azsecrets.Client) {
	t.Helper()
	ctx := context.Background()

	req := testcontainers.ContainerRequest{
		// docker.io, not ghcr.io/nagyesta/lowkey-vault: ghcr returns "denied"
		// from this network. A real numbered tag, never "latest" -- Docker
		// Hub does not publish one for this image (verified locally). 7.3.74
		// specifically (not just any recent tag): it is the oldest
		// confirmed-working release found here -- 2.4.109, the first tag
		// tried, rejects azsecrets' api-version=7.6 with 400 Invalid request
		// parameters ("api-version=7.5 OR 7.4 OR 7.3 OR 7.2 not met"); 7.3.74
		// accepts it.
		Image: "docker.io/nagyesta/lowkey-vault:7.3.74",
		// Lowkey Vault registers each auto-provisioned vault's URI using the
		// port it is listening on *inside* the container (8443) at startup,
		// and (from 7.x onward) enforces strict matching between that
		// registered URI and the Host header of incoming requests. A
		// testcontainers-assigned random host port would make our client's
		// URL (https://localhost:<random>) never match what the container
		// registered, so this pins the host port to the same 8443 instead
		// of letting testcontainers pick one.
		ExposedPorts: []string{"8443/tcp"},
		HostConfigModifier: func(hc *container.HostConfig) {
			hc.PortBindings = network.PortMap{
				network.MustParsePort("8443/tcp"): []network.PortBinding{
					{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: "8443"},
				},
			}
		},
		WaitingFor: wait.ForLog("Vaults registered!").WithStartupTimeout(60 * time.Second),
	}
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start lowkey vault container: %v", err)
	}
	t.Cleanup(func() {
		if err := ctr.Terminate(context.Background()); err != nil {
			t.Logf("terminate lowkey vault container: %v", err)
		}
	})

	port, err := ctr.MappedPort(ctx, "8443/tcp")
	if err != nil {
		t.Fatalf("lowkey vault container mapped port: %v", err)
	}

	// Lowkey Vault auto-registers a vault reachable via the "localhost"
	// hostname (confirmed via its startup log: "Creating vault for URI:
	// https://localhost:8443"); the vault is selected by the request's Host
	// header/SNI, not by any path segment, exactly like real Key Vault's
	// <name>.vault.azure.net convention. testcontainers publishes the
	// container's port to the host's loopback interface, so
	// https://localhost:<mappedPort>/ reaches this same auto-registered
	// vault regardless of what name our own code calls it internally.
	vaultURL := fmt.Sprintf("https://localhost:%s/", port.Port())

	// Self-signed cert (CN=lowkey-vault.local): trust it explicitly for this
	// test client rather than disabling verification process-wide.
	transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // test-only, talks to a local ephemeral container

	client, err := azsecrets.NewClient(vaultURL, stubCredential{}, &azsecrets.ClientOptions{
		ClientOptions: policy.ClientOptions{
			Transport: &http.Client{Transport: transport},
		},
		// The container's vault URL is https://localhost:<random-port>/, not
		// a *.vault.azure.net address, so the challenge-auth policy's
		// default check (the 401 challenge's "resource" claim must match
		// the vault's own domain) never matches here. Disabling it is safe
		// for this test-only client: stubCredential ignores the challenge's
		// resource/scope entirely anyway.
		DisableChallengeResourceVerification: true,
	})
	if err != nil {
		t.Fatalf("build lowkey vault azsecrets client: %v", err)
	}

	reader := &keyVaultReader{
		credential: stubCredential{},
		clients:    map[string]secretsClient{testVaultName: client},
	}
	return reader, client
}

func TestKeyVaultReader_GetLatest_HappyPath(t *testing.T) {
	reader, client := startLowkeyVault(t)
	ctx := context.Background()

	const secretName = "integration-happy-path"
	secretValue := "correct-horse-battery-staple"
	if _, err := client.SetSecret(ctx, secretName, azsecrets.SetSecretParameters{Value: &secretValue}, nil); err != nil {
		t.Fatalf("seed secret: %v", err)
	}

	value, version, err := reader.GetLatest(ctx, testVaultName, secretName)
	if err != nil {
		t.Fatalf("GetLatest() error = %v", err)
	}
	if value != secretValue {
		t.Fatalf("GetLatest() value = %q, want %q", value, secretValue)
	}
	if version == "" {
		t.Fatal("GetLatest() returned an empty version")
	}
}

func TestKeyVaultReader_GetLatest_NewVersionWins(t *testing.T) {
	reader, client := startLowkeyVault(t)
	ctx := context.Background()

	const secretName = "integration-new-version-wins"
	v1 := "first-value"
	if _, err := client.SetSecret(ctx, secretName, azsecrets.SetSecretParameters{Value: &v1}, nil); err != nil {
		t.Fatalf("seed v1: %v", err)
	}
	_, version1, err := reader.GetLatest(ctx, testVaultName, secretName)
	if err != nil {
		t.Fatalf("GetLatest() (v1) error = %v", err)
	}

	v2 := "second-value"
	if _, err := client.SetSecret(ctx, secretName, azsecrets.SetSecretParameters{Value: &v2}, nil); err != nil {
		t.Fatalf("seed v2: %v", err)
	}

	value, version2, err := reader.GetLatest(ctx, testVaultName, secretName)
	if err != nil {
		t.Fatalf("GetLatest() (v2) error = %v", err)
	}
	if value != v2 {
		t.Fatalf("GetLatest() after rotation = %q, want %q (latest-wins, FR-005)", value, v2)
	}
	if version2 == version1 {
		t.Fatalf("expected a new version identifier after rotation, got the same one twice: %q", version2)
	}
}

func TestKeyVaultReader_GetLatest_NotFound(t *testing.T) {
	reader, _ := startLowkeyVault(t)
	ctx := context.Background()

	_, _, err := reader.GetLatest(ctx, testVaultName, "does-not-exist")
	if !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("GetLatest() error = %v, want wrapped %v", err, ErrSecretNotFound)
	}
}

func TestKeyVaultReader_GetLatest_DisabledSecret(t *testing.T) {
	reader, client := startLowkeyVault(t)
	ctx := context.Background()

	const secretName = "integration-disabled"
	secretValue := "should-never-be-returned"
	setResp, err := client.SetSecret(ctx, secretName, azsecrets.SetSecretParameters{Value: &secretValue}, nil)
	if err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	version := setResp.ID.Version()

	enabled := false
	if _, err := client.UpdateSecretProperties(ctx, secretName, version,
		azsecrets.UpdateSecretPropertiesParameters{
			SecretAttributes: &azsecrets.SecretAttributes{Enabled: &enabled},
		}, nil); err != nil {
		t.Fatalf("disable secret: %v", err)
	}

	_, _, err = reader.GetLatest(ctx, testVaultName, secretName)
	if !errors.Is(err, ErrSecretDisabled) {
		t.Fatalf("GetLatest() error = %v, want wrapped %v", err, ErrSecretDisabled)
	}
	if err != nil && strings.Contains(err.Error(), secretValue) {
		t.Fatalf("GetLatest() error leaked the secret value: %q", err.Error())
	}
}
