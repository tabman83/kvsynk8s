//go:build e2e
// +build e2e

/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

// T032: the full sync loop (US1 + US2 + US3), proven against a kind cluster
// running the actually-built/deployed operator image -- no fakes, no
// bypassing the production code path -- talking to Azurite (real azqueue
// wire protocol, --oauth basic) and Lowkey Vault (real azsecrets wire
// protocol) as containers on the kind cluster's own Docker network.
//
// The one obstacle this needed to solve: cmd/main.go always builds its
// Azure clients via azidentity.NewDefaultAzureCredential, which needs *a*
// Microsoft Entra token before it will make any request at all. There is no
// flag or env var to swap in a different credential type, and this suite
// intentionally does not add one -- that would mean testing a code path the
// real deployment never uses. Instead this relies on two standard,
// unmodified azidentity/MSAL behaviors (verified interactively against
// these exact images before writing this suite; see
// testdata/authstub/main.go's package doc for the full explanation):
// AZURE_AUTHORITY_HOST redirects EnvironmentCredential's token requests to
// authstub, and AZURE_TENANT_ID=adfs makes MSAL skip the hardcoded call to
// real Azure AD's instance-discovery endpoint that would otherwise reject
// an unrecognized authority host. Neither emulator validates the resulting
// token's signature (Lowkey Vault does not check bearer tokens at all;
// Azurite's --oauth basic mode only checks claim shape/expiry/issuer
// prefix/audience -- verified from its own source, QueueTokenAuthenticator.
// js), so authstub's fabricated, unsigned token satisfies both.
//
// The other piece making this possible: internal/azure/keyvault.go's
// KVSYNK8S_KEYVAULT_TEST_ENDPOINT override (added for this task) lets this
// suite point the real SecretReader at Lowkey Vault's non-*.vault.azure.net
// address without the operator's vaultURL() construction ever changing for
// a real deployment (the env var is unset there, so behavior is identical
// to before this existed).
//
// Scope note: SC-005 (100-secret burst) is not exercised here -- T016's
// fake-QueueSource unit test already covers burst handling at the listener
// level, and repeating that against a live container adds runtime without
// covering new code. This suite proves the wire protocols and the
// single-change propagation path (SC-001) end to end instead.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azqueue/v2"

	"github.com/tabman83/kvsynk8s/test/utils"
)

const (
	// dockerNetworkEnv, when set, overrides the Docker network the emulator
	// containers join. kind creates a network literally named "kind" by
	// default (verified locally); KIND_EXPERIMENTAL_DOCKER_NETWORK is kind's
	// own env var for changing that, so this suite honors the same one
	// instead of inventing a second name for the same concept.
	dockerNetworkEnv     = "KIND_EXPERIMENTAL_DOCKER_NETWORK"
	defaultDockerNetwork = "kind"

	azuriteImage     = "mcr.microsoft.com/azure-storage/azurite:3.36.0"
	lowkeyVaultImage = "docker.io/nagyesta/lowkey-vault:7.3.74"

	azuriteContainerName  = "kvsynk8s-e2e-azurite"
	lowkeyContainerName   = "kvsynk8s-e2e-lowkeyvault"
	authstubContainerName = "kvsynk8s-e2e-authstub"
	authstubImageTag      = "kvsynk8s-e2e-authstub:e2e"

	azuriteAlias = "azurite"
	// authstubAlias deliberately has a dot in it, and deliberately does NOT
	// end in ".local". Two real, distinct problems were found and fixed
	// here during development, both confirmed with `getent hosts` run
	// inside a cluster pod (not guessed from symptoms):
	//   1. A bare single-label alias ("authstub") intermittently failed
	//      cluster DNS resolution outright ("server misbehaving" --
	//      Go's pure-Go resolver, CGO_ENABLED=0 in the Dockerfile, does
	//      not fall back to the bare name past an intermediate SERVFAIL
	//      the way a libc resolver does).
	//   2. Switching to "authstub.local" traded that for a worse, 100%
	//      reproducible failure: kind nodes resolve ".local" names via
	//      mDNS (RFC 6762), not ordinary DNS, and nothing here answers
	//      mDNS queries, so `getent hosts authstub.local` failed outright
	//      while `getent hosts azurite` and `getent hosts
	//      default.lowkey-vault` (below) succeeded on the very same pod.
	// A two-label alias that is neither bare nor ".local" -- matching
	// lowkeyVaultAlias's own already-working shape -- resolves cleanly.
	authstubAlias = "authstub.e2e"
	// lowkeyVaultAlias is one of Lowkey Vault's own built-in
	// auto-registered vault hostnames (confirmed from its startup log:
	// "Creating vault for URI: https://default.lowkey-vault:8443"),
	// reachable at this exact Docker network alias -- not something this
	// suite configures on the Lowkey Vault side, only on the Docker network
	// side.
	lowkeyVaultAlias = "default.lowkey-vault"

	// Why Docker network aliases at all, rather than a headless Service with
	// hand-written Endpoints pointing at the container IPs?
	//
	// A Service would be better: CoreDNS would answer authoritatively out of
	// cluster.local, so these names would never be forwarded to the public
	// internet, never be cached as non-cluster names for 30s, and never depend
	// on Docker's embedded resolver. Several past failures in this suite came
	// from that path.
	//
	// It is NOT blocked. An earlier version of this comment claimed Lowkey Vault
	// could only ever serve the hostnames it auto-registers, so a cluster DNS
	// name could not address it. That was wrong, and it was wrong because it was
	// inferred from the startup log alone. The pinned image takes
	// LOWKEY_VAULT_ALIASES, reachable through its entrypoint
	// (sh -c "java ${JAVA_OPTS} -jar /lowkey-vault.jar ${LOWKEY_ARGS}"), so one
	// extra -e on the docker run below is enough:
	//
	//   -e LOWKEY_ARGS=--LOWKEY_VAULT_ALIASES=default.lowkey-vault=lowkey-vault.default.svc.cluster.local:8443
	//
	// Verified end to end against docker.io/nagyesta/lowkey-vault:7.3.74: it logs
	// "Updating aliases of: https://default.lowkey-vault:8443 , adding:
	// https://lowkey-vault.default.svc.cluster.local:8443", then serves that name
	// with FULL TLS verification against its own shipped certificate, whose SAN
	// list already includes DNS:*.default.svc.cluster.local. A secret written via
	// the alias reads back with the same version id via the original host, so it
	// is one vault with two authorities. An un-aliased host on the same wildcard
	// cert returns "Unable to find active vault", so the resolution is real.
	//
	// The alias is an AUTHORITY, port included: the value above works and the
	// same name on another port 404s. A headless Service would therefore have to
	// expose 8443.
	//
	// So this is a deferred choice, not an impossibility. Not done here because
	// it means Services plus hand-maintained EndpointSlices for three containers,
	// new SANs for the azurite and authstub certs, and keeping all of that in
	// step as containers come and go -- a sizeable change to a suite that is
	// currently stable, to remove a class of failure that is no longer biting now
	// that authstub stays up and the host-side dials are pinned to 127.0.0.1.
	// Worth doing the next time this path causes trouble; the recipe above is the
	// hard part and it is already proven.

	// testVaultName is the SecretSync spec.vault.name this suite uses. It
	// has nothing to do with lowkeyVaultAlias: KVSYNK8S_KEYVAULT_TEST_ENDPOINT
	// (below) makes the operator's real vaultURL() construction irrelevant
	// for this test run, so spec.vault.name only has to satisfy the CRD's
	// pattern (which lowkeyVaultAlias, containing dots, does not).
	testVaultName = "lowkeyvault"

	azuriteAccountName = "devstoreaccount1"
	azuriteAccountKey  = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="
	e2eQueueName       = "kv-e2e-notifications"

	// keyVaultAPIVersion matches what azsecrets v1.5.0 (this repo's pinned
	// SDK version) actually sends, so this suite's direct HTTP calls to
	// seed/rotate Lowkey Vault secrets exercise the same REST surface the
	// operator's real client does.
	keyVaultAPIVersion = "7.6"

	e2eCAConfigMap = "kvsynk8s-e2e-ca"
)

// dockerNetwork returns the Docker network the emulator containers and the
// kind cluster's nodes both need to be on.
func dockerNetwork() string {
	if v := os.Getenv(dockerNetworkEnv); v != "" {
		return v
	}
	return defaultDockerNetwork
}

// generateSelfSignedCert returns a PEM-encoded self-signed certificate and
// key valid for the given hostnames, used as the TLS identity for the
// Azurite and authstub containers this suite starts. One shared keypair
// covers both: it is used purely as a server certificate for each
// independently, never as a real CA.
func generateSelfSignedCert(hosts []string) (certPEM, keyPEM []byte, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, fmt.Errorf("generate key: %w", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "kvsynk8s-e2e"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(6 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("create certificate: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return certPEM, keyPEM, nil
}

// fetchPeerCertPEM connects to hostPort over TLS without verifying the
// server's identity (there is nothing to verify it against yet -- fetching
// its certificate is the whole point) and returns that certificate
// PEM-encoded. Used once, at setup, to learn Lowkey Vault's own built-in
// self-signed certificate so it can be added to the bundle the operator is
// told to trust.
func fetchPeerCertPEM(hostPort string) ([]byte, error) {
	// nolint:gosec // deliberately fetching an otherwise-unknown cert to trust it
	conn, err := tls.Dial("tcp", hostPort, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", hostPort, err)
	}
	defer func() { _ = conn.Close() }()
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return nil, fmt.Errorf("no peer certificate presented by %s", hostPort)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certs[0].Raw}), nil
}

// removeContainer best-effort removes a container this suite started;
// safe to call even if it was never created.
func removeContainer(name string) {
	cmd := exec.Command("docker", "rm", "-f", name)
	_, _ = utils.Run(cmd)
}

// hostPortOf returns the host port Docker published containerPort/tcp to
// for the named container.
// hostPortOf returns the host port a container's port is published on, always
// picking the IPv4 binding.
//
// Docker publishes `-p 0:PORT` on both 0.0.0.0 and :: where the host is dual
// stack, so the bindings list can hold two entries and blindly taking index 0
// can hand back the IPv6 one. Callers connect to 127.0.0.1 for the same reason:
// on a host where "localhost" resolves to ::1 first (GitHub Actions runners do,
// this developer machine does not) dialling "localhost:<port>" reaches Docker
// over IPv6 and gets "connection reset by peer" the moment TLS starts. Using an
// explicit IPv4 literal on both sides removes the whole question.
func hostPortOf(name, containerPort string) (string, error) {
	cmd := exec.Command("docker", "inspect",
		"-f", fmt.Sprintf(`{{ range (index .NetworkSettings.Ports "%s/tcp") }}{{ .HostIp }} {{ .HostPort }}{{ "\n" }}{{ end }}`, containerPort),
		name)
	out, err := utils.Run(cmd)
	if err != nil {
		return "", err
	}
	var fallback string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		hostIP, hostPort := fields[0], fields[1]
		if ip := net.ParseIP(hostIP); ip != nil && ip.To4() != nil {
			return hostPort, nil
		}
		if fallback == "" {
			fallback = hostPort
		}
	}
	if fallback != "" {
		return fallback, nil
	}
	return "", fmt.Errorf("no published host port for %s/tcp on container %s (docker inspect gave %q)",
		containerPort, name, strings.TrimSpace(out))
}

// newLowkeyVaultHTTPClient returns an http.Client for this suite's own
// setup/assertion traffic to Lowkey Vault (seeding a secret, rotating it) --
// never used by the operator itself, which always validates against the
// real fetched certificate via SSL_CERT_FILE and reaches Lowkey Vault
// through the kind Docker network directly.
//
// This test process is not itself attached to that Docker network, so it
// reaches the container through its host-published port instead -- but
// Lowkey Vault identifies which vault a request is for by the Host header/
// SNI, not by network path (confirmed interactively before writing this
// suite), and the operator always addresses it as lowkeyVaultAlias. So this
// client keeps that same Host header/SNI on every request (the URLs this
// file builds all start with https://<lowkeyVaultAlias>:8443/) while
// quietly dialing the host-published port instead of relying on Docker's
// embedded DNS, which is unreachable from outside any container on that
// network.
func newLowkeyVaultHTTPClient(lowkeyHostPort string) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	redirectAddr := lowkeyVaultAlias + ":8443"
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				if addr == redirectAddr {
					addr = lowkeyHostPort
				}
				return dialer.DialContext(ctx, network, addr)
			},
		},
	}
}

// lowkeyVaultSetSecret creates or rotates a secret directly against Lowkey
// Vault's REST API (bearer token content is never validated -- see
// testdata/authstub/main.go's package doc) and returns the new version.
func lowkeyVaultSetSecret(client *http.Client, baseURL, name, value string) (version string, err error) {
	body, _ := json.Marshal(map[string]string{"value": value})
	url := fmt.Sprintf("%s/secrets/%s?api-version=%s", strings.TrimSuffix(baseURL, "/"), name, keyVaultAPIVersion)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer e2e-setup-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("set secret %s: %d: %s", name, resp.StatusCode, string(respBody))
	}
	var parsed struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("parse set-secret response: %w", err)
	}
	segments := strings.Split(parsed.ID, "/")
	return segments[len(segments)-1], nil
}

// newAzuriteQueueClient builds a shared-key-authenticated azqueue client
// against Azurite's host-published port, using devstoreaccount1's
// well-known development key (public, documented by Microsoft -- not a
// secret). This is the suite's own setup/injection path; the operator
// itself always goes through the OAuth path proven by authstub.
func newAzuriteQueueClient(hostPort string) (*azqueue.QueueClient, error) {
	cred, err := azqueue.NewSharedKeyCredential(azuriteAccountName, azuriteAccountKey)
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("https://%s/%s/%s", hostPort, azuriteAccountName, e2eQueueName)
	return azqueue.NewQueueClientWithSharedKeyCredential(url, cred, nil)
}

// eventGridSecretNewVersionMessage builds the Base64-encoded Event Grid
// queue message body contracts/queue-message.md documents for a
// SecretNewVersionCreated notification.
func eventGridSecretNewVersionMessage(eventID, vaultName, secretName, version string) string {
	body := fmt.Sprintf(`{
		"id": %q,
		"topic": "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/Microsoft.KeyVault/vaults/e2e-vault",
		"subject": %q,
		"eventType": "Microsoft.KeyVault.SecretNewVersionCreated",
		"eventTime": "2026-01-01T00:00:00.0000000Z",
		"dataVersion": "1",
		"metadataVersion": "1",
		"data": {
			"Id": "https://e2e-vault.vault.azure.net/secrets/%s/%s",
			"VaultName": %q,
			"ObjectType": "secret",
			"ObjectName": %q,
			"Version": %q,
			"NBF": null,
			"EXP": null
		}
	}`, eventID, secretName, secretName, version, vaultName, secretName, version)
	return base64.StdEncoding.EncodeToString([]byte(body))
}

// waitForSecretDataValue polls until the named Secret's key holds want, or
// fails the spec after timeout. Returns the elapsed duration so callers can
// assert SC-001's <60s bound explicitly.
func waitForSecretDataValue(namespace, name, key, want string, timeout time.Duration) time.Duration {
	start := time.Now()
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "secret", name, "-n", namespace,
			"-o", fmt.Sprintf("jsonpath={.data.%s}", key))
		out, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(out))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(string(decoded)).To(Equal(want))
	}, timeout, time.Second).Should(Succeed())
	return time.Since(start)
}

// waitForSyncedEvent polls until at least one "Synced" Event recorded on the
// named SecretSync is visible, or fails the spec after timeout. This is a
// positive assertion on the whole event pipeline: the controller records
// through the events.k8s.io/v1 API, and kind enforces RBAC, so this fails if
// the ClusterRole grants the wrong API group (the bug where the grant was on
// the core group and every Eventf got 403, which the broadcaster only logs).
// Events written via events.k8s.io/v1 are served by the core v1 events API
// too (shared storage), so the core field selectors below see them.
func waitForSyncedEvent(namespace, name string, timeout time.Duration) {
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "events", "-n", namespace,
			"--field-selector",
			fmt.Sprintf("involvedObject.kind=SecretSync,involvedObject.name=%s,reason=Synced", name),
			"-o", "jsonpath={.items[*].reason}")
		out, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(out).To(ContainSubstring("Synced"),
			"no Synced event recorded on SecretSync %s/%s", namespace, name)
	}, timeout, time.Second).Should(Succeed())
}

// applySecretSync applies a minimal SecretSync manifest via kubectl,
// matching how quickstart.md and every other declarative resource in this
// suite is managed.
func applySecretSync(namespace, name, vaultName, secretName string) {
	manifest := fmt.Sprintf(`apiVersion: kvsynk8s.io/v1alpha1
kind: SecretSync
metadata:
  name: %s
  namespace: %s
spec:
  vault:
    name: %s
    secret: %s
`, name, namespace, vaultName, secretName)
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "failed to apply SecretSync %s", name)
}

func deleteSecretSync(namespace, name string) {
	// --timeout bounds the wait for the operator to clear the finalizer. The
	// result is reported rather than silently dropped: a SecretSync that does
	// not finish deleting here is what deadlocks teardown later (see
	// drainSecretSyncs), so it needs to be visible in the log.
	cmd := exec.Command("kubectl", "delete", "secretsync", name, "-n", namespace,
		"--ignore-not-found", "--timeout=60s")
	if _, err := utils.Run(cmd); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "deleting SecretSync %s/%s did not complete: %v\n", namespace, name, err)
	}
}

// drainSecretSyncs removes every SecretSync in the cluster while the operator
// is still running, and guarantees none is left holding a finalizer.
//
// Every SecretSync carries kvsynk8s.io/secretsync-finalizer, which only the
// running operator ever clears. Teardown removes the operator Deployment, the
// CRD and the namespace, so if a single SecretSync survives into that step with
// its finalizer still set, nothing is left that can clear it: the namespace
// sticks in Terminating and kubectl blocks until the test binary is killed.
// That is a 30 minute hang with no failure message, not a test failure. The
// suite hit exactly this once the emulator scenarios stopped being skipped.
//
// Deliberately cluster-wide and callable at any point: it is invoked both from
// the T032 AfterAll (early, while the operator is definitely healthy) and from
// the outer AfterAll immediately before `make undeploy` (the backstop that
// covers any spec, present or future, wherever it put its objects). It is
// idempotent, so running it twice costs one no-op kubectl call.
func drainSecretSyncs() {
	// Nothing to do if the CRD is not installed -- kubectl would error rather
	// than return an empty list.
	if _, err := utils.Run(exec.Command("kubectl", "get", "crd", "secretsyncs.kvsynk8s.io")); err != nil {
		return
	}

	out, err := utils.Run(exec.Command("kubectl", "get", "secretsync", "--all-namespaces",
		"-o", `jsonpath={range .items[*]}{.metadata.namespace} {.metadata.name}{"\n"}{end}`))
	if err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "could not list SecretSyncs to drain: %v\n", err)
		return
	}

	type ref struct{ ns, name string }
	var refs []ref
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if f := strings.Fields(line); len(f) == 2 {
			refs = append(refs, ref{f[0], f[1]})
		}
	}
	if len(refs) == 0 {
		return
	}

	for _, r := range refs {
		cmd := exec.Command("kubectl", "-n", r.ns, "delete", "secretsync", r.name,
			"--ignore-not-found", "--timeout=60s")
		if _, err := utils.Run(cmd); err != nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "deleting SecretSync %s/%s did not complete: %v\n", r.ns, r.name, err)
		}
	}

	// Belt and braces. Anything still standing gets its finalizer stripped, so
	// teardown degrades to a logged warning instead of a deadlock. Safe here:
	// the managed Secret is owned by the SecretSync and garbage collected, and
	// the cluster is thrown away at the end of the run either way.
	out, err = utils.Run(exec.Command("kubectl", "get", "secretsync", "--all-namespaces",
		"-o", `jsonpath={range .items[*]}{.metadata.namespace} {.metadata.name}{"\n"}{end}`))
	if err != nil {
		return
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		_, _ = fmt.Fprintf(GinkgoWriter,
			"SecretSync %s/%s survived deletion; clearing its finalizer so teardown cannot deadlock\n", f[0], f[1])
		_, _ = utils.Run(exec.Command("kubectl", "-n", f[0], "patch", "secretsync", f[1],
			"--type=merge", "-p", `{"metadata":{"finalizers":null}}`))
		forciblyFinalized = append(forciblyFinalized, f[0]+"/"+f[1])
	}
}

// forciblyFinalized records every SecretSync whose finalizer this suite had to
// strip by hand. It should always be empty.
//
// Stripping keeps teardown from deadlocking, but on its own it would downgrade
// a real operator defect -- the controller failing to finalize a SecretSync --
// from an obvious 30 minute hang into one warning line under a green suite.
// That is a worse failure mode than the one being fixed, so the outer AfterAll
// fails the run if this is not empty: teardown still completes, and the defect
// is still reported.
var forciblyFinalized []string

// failIfFinalizersWereForced fails the suite if drainSecretSyncs had to strip
// any finalizer. Call it LAST in teardown, so cleanup finishes first.
func failIfFinalizersWereForced() {
	if len(forciblyFinalized) == 0 {
		return
	}
	Fail(fmt.Sprintf(
		"the operator did not clear the finalizer on %d SecretSync(s): %s.\n"+
			"They were force-finalized so teardown could finish, but this means reconcileDelete "+
			"did not complete for them -- treat it as an operator bug, not a test-cleanup detail.",
		len(forciblyFinalized), strings.Join(forciblyFinalized, ", ")))
}

// expectContainerRunning fails immediately, with the container's own logs in
// the message, if a container is not running a moment after `docker run -d`
// returned success.
//
// `docker run -d` only reports that the container was CREATED. If the process
// inside exits straight away -- a missing file, an unreadable mount, a bad
// flag -- docker still exits 0 and the test carries on against a container
// that is already dead. That is exactly how an unreadable TLS key inside
// authstub spent a dozen runs looking like a cluster-DNS fault: the name
// briefly failed to resolve (curl exit 6) and then resolved to a container
// with nothing listening (curl exit 7), which reads like a network problem
// unless you happen to check whether the container is still alive.
//
// Anything that starts a container in this suite must go through here.
func expectContainerRunning(name string) {
	GinkgoHelper()
	cmd := exec.Command("docker", "inspect", "--format", "{{.State.Running}} {{.State.ExitCode}}", name)
	out, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "could not inspect container %s", name)
	if strings.HasPrefix(strings.TrimSpace(out), "true") {
		return
	}
	logsOut, _ := utils.Run(exec.Command("docker", "logs", "--tail", "50", name))
	Fail(fmt.Sprintf(
		"container %s is not running (docker inspect gave %q) -- it exited instead of serving.\n"+
			"Last 50 log lines:\n%s", name, strings.TrimSpace(out), logsOut))
}

// netcheckPodName is a throwaway pod (default namespace: unlike kvsynk8s it
// carries no restricted PodSecurity label, so a bare curl image needs no
// securityContext) used only to prove the emulator containers are reachable
// over the cluster's own pod network/DNS before the operator is pointed at
// them.
const netcheckPodName = "kvsynk8s-e2e-netcheck"

// ensureNetcheckPod makes sure the shared in-cluster curl pod exists and is
// Ready, creating it the first time it is called. Reused across every
// in-cluster reachability check in this Context instead of spinning up a
// fresh pod each time.
func ensureNetcheckPod() {
	cmd := exec.Command("kubectl", "-n", "default", "get", "pod", netcheckPodName)
	if _, err := utils.Run(cmd); err == nil {
		return // already exists and (per prior call) already Ready
	}

	cmd = exec.Command("kubectl", "-n", "default", "run", netcheckPodName,
		"--image=curlimages/curl:8.21.0", "--restart=Never", "--", "sleep", "600")
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() {
		cmd := exec.Command("kubectl", "-n", "default", "delete", "pod", netcheckPodName, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	})

	cmd = exec.Command("kubectl", "-n", "default", "wait", "--for=condition=Ready",
		"pod/"+netcheckPodName, "--timeout=60s")
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())
}

// curlReachable makes one attempt, from inside the cluster, to reach target
// (through a Docker network alias, exactly as the operator will reach it).
// nil means some HTTP response came back at all -- even 400/401/404 proves
// the TCP+TLS path works; this only checks reachability, never that the
// emulator likes the deliberately bare request.
func curlReachable(target string) error {
	cmd := exec.Command("kubectl", "-n", "default", "exec", netcheckPodName, "--",
		"curl", "-sk", "-o", "/dev/null", "-w", "%{http_code}", "--max-time", "5", target)
	out, err := utils.Run(cmd)
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) == "" {
		return fmt.Errorf("empty response reaching %s", target)
	}
	return nil
}

// waitForInClusterReachability retries curlReachable against all three
// emulators until every one succeeds or the deadline passes.
func waitForInClusterReachability() {
	ensureNetcheckPod()

	targets := []string{
		fmt.Sprintf("https://%s:10001/%s/x?restype=queue", azuriteAlias, azuriteAccountName),
		fmt.Sprintf("https://%s:9911/adfs/.well-known/openid-configuration", authstubAlias),
		fmt.Sprintf("https://%s:8443/secrets/x?api-version=%s", lowkeyVaultAlias, keyVaultAPIVersion),
	}
	for _, target := range targets {
		Eventually(func() error {
			return curlReachable(target)
		}, 60*time.Second, 2*time.Second).Should(
			Succeed(), "target never became reachable from inside the cluster: %s", target)
	}
}

func secretSyncStatusField(namespace, name, field string) (string, error) {
	cmd := exec.Command("kubectl", "get", "secretsync", name, "-n", namespace,
		"-o", fmt.Sprintf("jsonpath={.status.%s}", field))
	return utils.Run(cmd)
}

// registerSecretSyncEmulatorTests wires T032's emulator-backed scenarios
// into the suite. Called from within the outer "Manager" Describe so it
// shares that block's namespace/CRD/operator deployment (BeforeAll above it
// has already run) rather than standing up a second copy of everything.
func registerSecretSyncEmulatorTests() {
	Context("SecretSync sync loop against real Azurite/Lowkey Vault (T032)", Ordered, func() {
		var (
			certDir          string
			azuritePort      string
			lowkeyPort       string
			queueClient      *azqueue.QueueClient
			lowkeyBase       string
			lowkeyHTTPClient *http.Client
		)

		BeforeAll(func() {
			// Runs by default. This Context was opt-in behind
			// KVSYNK8S_E2E_EMULATORS=1 for a long time because it failed
			// consistently under `make test-e2e` while the same commands run by
			// hand always worked, and the cause was not found. It has been
			// found: authstub was dying on startup, unable to read its TLS
			// keypair as a non-root uid, and the suite reported that as the
			// container being unreachable over cluster DNS. See the certDir and
			// expectContainerRunning comments. Nothing about DNS was wrong.

			network := dockerNetwork()

			By("generating a shared TLS certificate for the azurite/authstub emulators")
			var err error
			certDir, err = os.MkdirTemp("", "kvsynk8s-e2e-certs-*")
			Expect(err).NotTo(HaveOccurred())
			// This directory is bind-mounted into the emulator containers, and
			// authstub runs as the distroless nonroot uid (65532) rather than
			// root. os.MkdirTemp creates the directory 0700 and a private key
			// would normally be written 0600 -- both owned by the host user, so
			// uid 65532 can neither traverse the directory nor read the key, and
			// authstub dies on startup. azurite hides this because it runs as
			// root, and Lowkey Vault hides it because it mounts no certs at all.
			//
			// So: world-readable on purpose. This is a throwaway self-signed
			// keypair, generated per run into a temp dir, used only by two
			// emulators inside this suite, and thrown away with the directory. It
			// is not a credential for anything. Do not copy this pattern outside
			// test/e2e.
			Expect(os.Chmod(certDir, 0o755)).To(Succeed())
			certPEM, keyPEM, err := generateSelfSignedCert([]string{azuriteAlias, authstubAlias, "localhost", "127.0.0.1"})
			Expect(err).NotTo(HaveOccurred())
			Expect(os.WriteFile(filepath.Join(certDir, "cert.pem"), certPEM, 0o644)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(certDir, "key.pem"), keyPEM, 0o644)).To(Succeed())

			By("building the authstub image (a throwaway Entra ID token-endpoint stub, see its package doc)")
			projectDir, err := utils.GetProjectDir()
			Expect(err).NotTo(HaveOccurred())
			cmd := exec.Command("docker", "build", "-t", authstubImageTag,
				filepath.Join(projectDir, "test", "e2e", "testdata", "authstub"))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("starting azurite in --oauth basic mode")
			removeContainer(azuriteContainerName)
			cmd = exec.Command("docker", "run", "-d", "--name", azuriteContainerName,
				"--network", network, "--network-alias", azuriteAlias,
				"-p", "0:10001",
				"-v", certDir+":/certs",
				azuriteImage,
				"azurite", "--queueHost", "0.0.0.0", "--oauth", "basic",
				"--cert", "/certs/cert.pem", "--key", "/certs/key.pem",
				"--skipApiVersionCheck", "--silent")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			expectContainerRunning(azuriteContainerName)

			By("starting lowkey vault")
			removeContainer(lowkeyContainerName)
			cmd = exec.Command("docker", "run", "-d", "--name", lowkeyContainerName,
				"--network", network, "--network-alias", lowkeyVaultAlias,
				"-p", "0:8443",
				lowkeyVaultImage)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			expectContainerRunning(lowkeyContainerName)

			By("starting the auth stub")
			// This container used to be recreated up to 3 times, on the theory
			// that it hit an intermittent cluster-DNS cold start. It did not.
			// It was dying on startup every single time, because it is the only
			// container here that both bind-mounts certDir and runs as a
			// non-root uid, and the directory and key were not readable by that
			// uid (see the certDir comment above). Docker still exited 0, so the
			// suite went on to spend 90 seconds curling a dead container and
			// reporting it as a network fault. Recreating it three times just
			// repeated the same failure three times.
			//
			// With the permissions fixed and expectContainerRunning below, one
			// start is enough, and if it ever does fail to boot again the error
			// says so immediately and quotes the container's own log.
			ensureNetcheckPod()
			removeContainer(authstubContainerName)
			cmd = exec.Command("docker", "run", "-d", "--name", authstubContainerName,
				"--network", network, "--network-alias", authstubAlias,
				"-v", certDir+":/certs",
				authstubImageTag)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			expectContainerRunning(authstubContainerName)

			By("waiting for the emulators to accept connections")
			azuritePort, err = hostPortOf(azuriteContainerName, "10001")
			Expect(err).NotTo(HaveOccurred())
			lowkeyPort, err = hostPortOf(lowkeyContainerName, "8443")
			Expect(err).NotTo(HaveOccurred())
			lowkeyHTTPClient = newLowkeyVaultHTTPClient("127.0.0.1:" + lowkeyPort)
			Eventually(func() error {
				_, err := fetchPeerCertPEM("127.0.0.1:" + azuritePort)
				return err
			}, 30*time.Second, time.Second).Should(Succeed())
			var lowkeyCertPEM []byte
			Eventually(func() error {
				var err error
				lowkeyCertPEM, err = fetchPeerCertPEM("127.0.0.1:" + lowkeyPort)
				return err
			}, 30*time.Second, time.Second).Should(Succeed())

			// The checks above only prove the emulators accept TCP/TLS
			// connections from the *host* (via their published ports). The
			// operator reaches them from *inside* the cluster instead, over
			// the CNI's pod network and cluster DNS -- a separate path that
			// was observed, during development, to occasionally still be
			// warming up (fresh container network aliases, fresh pod
			// routes) even after the host-side checks above already
			// succeeded, which surfaced as azidentity token requests
			// timing out from the operator pod the moment it started. This
			// proves the in-cluster path specifically before the operator
			// ever depends on it.
			By("waiting for the emulators to be reachable from inside the cluster")
			waitForInClusterReachability()

			By("creating the notification queue in azurite")
			queueClient, err = newAzuriteQueueClient("127.0.0.1:" + azuritePort)
			Expect(err).NotTo(HaveOccurred())
			// Unsetenv runs before the error assertion (same ordering as the
			// SC-001 enqueue below): a failed Create must not leave
			// SSL_CERT_FILE leaked into every later spec in this process.
			Expect(os.Setenv("SSL_CERT_FILE", filepath.Join(certDir, "cert.pem"))).To(Succeed())
			_, err = queueClient.Create(context.Background(), nil)
			Expect(os.Unsetenv("SSL_CERT_FILE")).To(Succeed())
			Expect(err).NotTo(HaveOccurred())

			By("writing the combined CA bundle the operator will trust")
			bundle := append(append([]byte{}, certPEM...), lowkeyCertPEM...)
			bundlePath := filepath.Join(certDir, "ca-bundle.pem")
			Expect(os.WriteFile(bundlePath, bundle, 0o644)).To(Succeed())

			By("creating the ConfigMap holding that CA bundle")
			cmd = exec.Command("kubectl", "-n", namespace, "create", "configmap", e2eCAConfigMap,
				"--from-file=ca-bundle.pem="+bundlePath)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("pointing the operator at the emulators")
			// This also drops config/manager/manager.yaml's 200m CPU limit
			// for this patched instance only (the checked-in manifest real
			// deployments use is untouched). That limit is sized for an
			// idle production operator; under it, this suite's first
			// requests -- TLS handshakes to three freshly-started
			// containers plus the manager's own startup burst (informer
			// sync, metrics server, health probes) all landing at once --
			// were CPU-throttled badly enough that azidentity's token
			// requests to authstub hit "context deadline exceeded" on
			// every attempt, reproduced consistently across multiple runs
			// even though the identical request from an unthrottled pod
			// completed in single-digit milliseconds.
			//
			// One atomic patch, not two: patching env and then volumes as
			// separate calls each triggers its own rollout, so the pod that
			// briefly exists after the first patch has SSL_CERT_FILE set but
			// no volume yet to satisfy it. A single patch means exactly one
			// rollout, straight to the fully-configured pod.
			lowkeyBase = fmt.Sprintf("https://%s:8443", lowkeyVaultAlias)
			patch := fmt.Sprintf(`[
				{"op":"add","path":"/spec/template/spec/containers/0/env","value":[
					{"name":"QUEUE_URL","value":"https://%s:10001/%s/%s"},
					{"name":"AZURE_AUTHORITY_HOST","value":"https://%s:9911/"},
					{"name":"AZURE_TENANT_ID","value":"adfs"},
					{"name":"AZURE_CLIENT_ID","value":"kvsynk8s-e2e"},
					{"name":"AZURE_CLIENT_SECRET","value":"kvsynk8s-e2e"},
					{"name":"SSL_CERT_FILE","value":"/etc/e2e-ca/ca-bundle.pem"},
					{"name":"KVSYNK8S_KEYVAULT_TEST_ENDPOINT","value":"%s/"},
					{"name":"RECONCILE_INTERVAL","value":"10m"}
				]},
				{"op":"add","path":"/spec/template/spec/volumes","value":[{"name":"e2e-ca","configMap":{"name":%q}}]},
				{"op":"add","path":"/spec/template/spec/containers/0/volumeMounts","value":[{"name":"e2e-ca","mountPath":"/etc/e2e-ca","readOnly":true}]},
				{"op":"replace","path":"/spec/template/spec/containers/0/resources","value":{"requests":{"cpu":"250m","memory":"64Mi"},"limits":{"memory":"256Mi"}}}
			]`, azuriteAlias, azuriteAccountName, e2eQueueName, authstubAlias, lowkeyBase, e2eCAConfigMap)
			cmd = exec.Command("kubectl", "-n", namespace, "patch", "deployment", "kvsynk8s-operator",
				"--type=json", "-p", patch)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for the patched operator pod to become ready")
			cmd = exec.Command("kubectl", "-n", namespace, "rollout", "status", "deployment/kvsynk8s-operator", "--timeout=120s")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			// The readiness probe only proves the health server answers; it
			// says nothing about whether this brand-new pod's DNS/CNI path
			// to the emulator containers is fully settled yet (observed
			// once during development: a fresh pod's very first outbound
			// call timed out even though the same call from an
			// already-running pod, or from this same pod moments later,
			// worked in single-digit milliseconds). A short settle window
			// costs little against this suite's overall runtime and avoids
			// that first-call flake.
			time.Sleep(10 * time.Second)
		})

		AfterAll(func() {
			// Must happen first, and while the operator is still running: see
			// drainSecretSyncs for what deadlocks otherwise.
			By("draining SecretSync objects while the operator can still finalize them")
			drainSecretSyncs()

			By("removing the emulator containers")
			removeContainer(azuriteContainerName)
			removeContainer(lowkeyContainerName)
			removeContainer(authstubContainerName)

			By("deleting the e2e CA configmap")
			cmd := exec.Command("kubectl", "-n", namespace, "delete", "configmap", e2eCAConfigMap, "--ignore-not-found")
			_, _ = utils.Run(cmd)

			if certDir != "" {
				_ = os.RemoveAll(certDir)
			}
		})

		AfterEach(func() {
			if !CurrentSpecReport().Failed() {
				return
			}
			By("fetching operator logs after a failure")
			cmd := exec.Command("kubectl", "-n", namespace, "logs", "-l", "control-plane=controller-manager", "--tail=200")
			out, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Operator logs:\n%s\n", out)
			}
		})

		It("creates a Secret with the vault value when a SecretSync is declared (US1)", func() {
			const secretName = "e2e-secret-us1"
			_, err := lowkeyVaultSetSecret(lowkeyHTTPClient, lowkeyBase, secretName, "e2e-first-value")
			Expect(err).NotTo(HaveOccurred())

			applySecretSync(namespace, secretName, testVaultName, secretName)
			DeferCleanup(func() { deleteSecretSync(namespace, secretName) })

			waitForSecretDataValue(namespace, secretName, secretName, "e2e-first-value", 90*time.Second)

			state, err := secretSyncStatusField(namespace, secretName, "state")
			Expect(err).NotTo(HaveOccurred())
			Expect(state).To(Equal("InSync"))

			By("checking a Synced event was recorded on the SecretSync (T027, FR-009)")
			waitForSyncedEvent(namespace, secretName, 30*time.Second)
		})

		It("propagates a secret rotation via the queue within 60s (SC-001, US2)", func() {
			const secretName = "e2e-secret-sc001"
			_, err := lowkeyVaultSetSecret(lowkeyHTTPClient, lowkeyBase, secretName, "e2e-value-v1")
			Expect(err).NotTo(HaveOccurred())

			applySecretSync(namespace, secretName, testVaultName, secretName)
			DeferCleanup(func() { deleteSecretSync(namespace, secretName) })
			waitForSecretDataValue(namespace, secretName, secretName, "e2e-value-v1", 90*time.Second)

			By("rotating the vault secret")
			newVersion, err := lowkeyVaultSetSecret(lowkeyHTTPClient, lowkeyBase, secretName, "e2e-value-v2")
			Expect(err).NotTo(HaveOccurred())

			By("injecting the matching SecretNewVersionCreated event into the queue")
			msg := eventGridSecretNewVersionMessage("e2e-evt-sc001", testVaultName, secretName, newVersion)
			Expect(os.Setenv("SSL_CERT_FILE", filepath.Join(certDir, "cert.pem"))).To(Succeed())
			_, err = queueClient.EnqueueMessage(context.Background(), msg, nil)
			Expect(os.Unsetenv("SSL_CERT_FILE")).To(Succeed())
			Expect(err).NotTo(HaveOccurred())

			start := time.Now()
			elapsed := waitForSecretDataValue(namespace, secretName, secretName, "e2e-value-v2", 60*time.Second)
			_, _ = fmt.Fprintf(GinkgoWriter, "SC-001: queue event to Secret update took %s (start %s)\n", elapsed, start)
			Expect(elapsed).To(BeNumerically("<", 60*time.Second))
		})

		It("re-creates a managed Secret deleted in-cluster (US3 drift repair)", func() {
			const secretName = "e2e-secret-drift"
			_, err := lowkeyVaultSetSecret(lowkeyHTTPClient, lowkeyBase, secretName, "e2e-drift-value")
			Expect(err).NotTo(HaveOccurred())

			applySecretSync(namespace, secretName, testVaultName, secretName)
			DeferCleanup(func() { deleteSecretSync(namespace, secretName) })
			waitForSecretDataValue(namespace, secretName, secretName, "e2e-drift-value", 90*time.Second)

			By("deleting the managed Secret directly")
			cmd := exec.Command("kubectl", "-n", namespace, "delete", "secret", secretName)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			waitForSecretDataValue(namespace, secretName, secretName, "e2e-drift-value", 30*time.Second)
		})

		It("removes the managed Secret when the SecretSync is deleted", func() {
			const secretName = "e2e-secret-deletion"
			_, err := lowkeyVaultSetSecret(lowkeyHTTPClient, lowkeyBase, secretName, "e2e-deletion-value")
			Expect(err).NotTo(HaveOccurred())

			applySecretSync(namespace, secretName, testVaultName, secretName)
			waitForSecretDataValue(namespace, secretName, secretName, "e2e-deletion-value", 90*time.Second)

			deleteSecretSync(namespace, secretName)

			Eventually(func() error {
				cmd := exec.Command("kubectl", "-n", namespace, "get", "secret", secretName)
				_, err := utils.Run(cmd)
				return err
			}, 30*time.Second, time.Second).Should(HaveOccurred(), "expected the managed Secret to be gone")
		})

		It("reports TargetConflict and leaves a pre-existing unmanaged Secret untouched", func() {
			const secretName = "e2e-secret-conflict"
			cmd := exec.Command("kubectl", "-n", namespace, "create", "secret", "generic", secretName,
				"--from-literal="+secretName+"=pre-existing-unmanaged-value")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() {
				cmd := exec.Command("kubectl", "-n", namespace, "delete", "secret", secretName, "--ignore-not-found")
				_, _ = utils.Run(cmd)
			})

			_, err = lowkeyVaultSetSecret(lowkeyHTTPClient, lowkeyBase, secretName, "e2e-conflict-vault-value")
			Expect(err).NotTo(HaveOccurred())

			applySecretSync(namespace, secretName, testVaultName, secretName)
			DeferCleanup(func() { deleteSecretSync(namespace, secretName) })

			Eventually(func(g Gomega) {
				reason, err := secretSyncStatusField(namespace, secretName, "reason")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(reason).To(Equal("TargetConflict"))
			}, 90*time.Second, time.Second).Should(Succeed())

			By("confirming the pre-existing Secret's value was never touched")
			cmd = exec.Command("kubectl", "-n", namespace, "get", "secret", secretName,
				"-o", fmt.Sprintf("jsonpath={.data.%s}", secretName))
			out, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(out))
			Expect(err).NotTo(HaveOccurred())
			Expect(string(decoded)).To(Equal("pre-existing-unmanaged-value"))
		})

		It("never leaks a planted sentinel value through operator logs or status (SC-004)", func() {
			const secretName = "e2e-secret-sentinel"
			sentinel := fmt.Sprintf("SENTINEL-e2e-%d-do-not-log-me", time.Now().UnixNano())
			_, err := lowkeyVaultSetSecret(lowkeyHTTPClient, lowkeyBase, secretName, sentinel)
			Expect(err).NotTo(HaveOccurred())

			applySecretSync(namespace, secretName, testVaultName, secretName)
			DeferCleanup(func() { deleteSecretSync(namespace, secretName) })
			waitForSecretDataValue(namespace, secretName, secretName, sentinel, 90*time.Second)

			By("scanning operator logs for the sentinel value")
			// --tail=-1 fetches the whole log: kubectl defaults to --tail=10
			// when a label selector is used, which would scan only the last
			// ten lines and let a leak earlier in the log pass unnoticed.
			cmd := exec.Command("kubectl", "-n", namespace, "logs", "-l", "control-plane=controller-manager", "--tail=-1")
			logs, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(logs).NotTo(ContainSubstring(sentinel))

			By("scanning the SecretSync status for the sentinel value")
			cmd = exec.Command("kubectl", "-n", namespace, "get", "secretsync", secretName, "-o", "json")
			statusJSON, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(statusJSON).NotTo(ContainSubstring(sentinel))

			By("waiting for the Synced event on this SecretSync, so the scan below runs against events that exist")
			// Without this, the absence check is trivially true when zero
			// events exist (exactly what happened while the RBAC grant was on
			// the wrong API group and every Eventf silently got 403).
			waitForSyncedEvent(namespace, secretName, 30*time.Second)

			By("scanning Kubernetes events for the sentinel value")
			cmd = exec.Command("kubectl", "-n", namespace, "get", "events", "-o", "json")
			eventsJSON, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(eventsJSON).NotTo(ContainSubstring(sentinel))
		})
	})
}
