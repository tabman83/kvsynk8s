// Copyright kvsynk8s contributors.
// SPDX-License-Identifier: Apache-2.0

// authstub is a minimal stand-in for Microsoft Entra ID's token endpoints,
// used only by test/e2e (T032). It exists to let the *actually deployed*
// kvsynk8s operator binary -- which authenticates via
// azidentity.NewDefaultAzureCredential and cannot be reconfigured with a
// different credential type from outside -- obtain *a* token in a
// kind cluster with no real Azure subscription available.
//
// This works because of two entirely standard, unmodified azidentity/MSAL
// behaviors, verified directly against this exact image before writing this
// program:
//  1. AZURE_AUTHORITY_HOST overrides the STS host EnvironmentCredential (one
//     of DefaultAzureCredential's chained credentials) talks to.
//  2. Setting AZURE_TENANT_ID=adfs makes MSAL skip Azure AD "instance
//     discovery" (a hardcoded call to the real login.microsoftonline.com
//     that would otherwise reject an unrecognized authority host) and use
//     the ADFS-style metadata document layout instead
//     (<authority>/adfs/.well-known/openid-configuration).
//
// The two emulators this unblocks (Azurite in --oauth basic mode, and
// Lowkey Vault) both accept the resulting token: Lowkey Vault does not
// validate bearer tokens at all (github.com/nagyesta/lowkey-vault docs:
// "will check whether [credentials] are there but ignore the value"), and
// Azurite's queue OAuth check (source: QueueTokenAuthenticator.js) only
// requires exp/nbf/iat to be present and iss to start with one of a few
// fixed real-Azure-AD prefixes and aud to match the storage resource --
// never a real signature, since it is a client-credentials-flow emulator,
// not an authorization server.
package main

import (
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"
)

// fixedIssuer must start with one of azqueue's real-Azure-AD issuer
// prefixes (Azurite's QueueTokenAuthenticator.js: VALID_ISSUE_PREFIXES) for
// the emitted token to pass Azurite's --oauth basic check. Azurite never
// contacts this issuer or validates a signature -- source-verified: it only
// checks the claim is a string with this prefix.
const fixedIssuer = "https://sts.windows.net/00000000-0000-0000-0000-000000000000/"

// fixedAudience matches azqueue's expected resource
// (VALID_QUEUE_AUDIENCES); Lowkey Vault does not check aud at all, so one
// fixed audience serves both emulators.
const fixedAudience = "https://storage.azure.com"

func b64url(v any) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}

// fakeToken returns an unsigned, JWT-shaped access token: three
// dot-separated base64url segments, decodable by any JWT parser, but never
// cryptographically verified by either emulator (see package doc). It is
// not a credential in any meaningful sense -- it authenticates nothing --
// which is exactly why this program must never run anywhere but a local,
// throwaway e2e cluster.
func fakeToken() string {
	now := time.Now().Unix()
	header := b64url(map[string]string{"alg": "RS256", "typ": "JWT"})
	payload := b64url(map[string]any{
		"aud": fixedAudience,
		"iss": fixedIssuer,
		"iat": now,
		"nbf": now,
		"exp": now + 3600,
		"sub": "kvsynk8s-e2e",
		"oid": "kvsynk8s-e2e",
	})
	return header + "." + payload + ".e2e-stub-unsigned"
}

func main() {
	addr := flag.String("addr", ":9911", "listen address")
	certFile := flag.String("cert", "/certs/cert.pem", "TLS certificate PEM path")
	keyFile := flag.String("key", "/certs/key.pem", "TLS key PEM path")
	flag.Parse()

	mux := http.NewServeMux()

	// ADFS-style OIDC discovery document. MSAL requests this because
	// AZURE_TENANT_ID=adfs makes it treat the authority as an ADFS
	// deployment (see package doc) instead of trying real Azure AD's
	// well-known layout.
	mux.HandleFunc("/adfs/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		base := "https://" + r.Host
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                base + "/adfs",
			"token_endpoint":                        base + "/adfs/oauth2/token",
			"authorization_endpoint":                base + "/adfs/oauth2/authorize",
			"jwks_uri":                              base + "/adfs/discovery/keys",
			"response_types_supported":              []string{"code", "id_token"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
			"token_endpoint_auth_methods_supported": []string{"client_secret_post", "client_secret_basic"},
		})
	})

	mux.HandleFunc("/adfs/oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token_type":   "Bearer",
			"expires_in":   3600,
			"access_token": fakeToken(),
		})
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, fmt.Sprintf("authstub: unhandled path %s", r.URL.Path), http.StatusNotFound)
	})

	srv := &http.Server{
		Addr:      *addr,
		Handler:   mux,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}
	// Read the keypair here rather than letting ListenAndServeTLS do it, so a
	// permission or path problem is reported as exactly that, before anything
	// claims to be serving. The old order logged "authstub listening on :9911"
	// and only then failed inside ListenAndServeTLS, so `docker logs` showed a
	// container that looked healthy and had in fact already exited -- which is
	// how an unreadable key spent a dozen e2e runs being investigated as a
	// cluster-DNS fault.
	cert, err := tls.LoadX509KeyPair(*certFile, *keyFile)
	if err != nil {
		log.Fatalf("authstub: cannot load keypair (cert=%s key=%s): %v", *certFile, *keyFile, err)
	}
	srv.TLSConfig.Certificates = []tls.Certificate{cert}

	log.Printf("authstub listening on %s", *addr)
	log.Fatal(srv.ListenAndServeTLS("", ""))
}
