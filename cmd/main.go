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

package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"net/url"
	"os"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	kvsynk8sv1alpha1 "github.com/tabman83/kvsynk8s/api/v1alpha1"
	"github.com/tabman83/kvsynk8s/internal/azure"
	"github.com/tabman83/kvsynk8s/internal/controller"
	"github.com/tabman83/kvsynk8s/internal/events"
	// +kubebuilder:scaffold:imports
)

// eventsChannelBufferSize sizes the channel the queue listener uses to hand
// matched SecretSync objects to the controller's source.Channel watch. A
// burst (SC-005: 100+ events/min) can match many SecretSyncs faster than
// they reconcile; a generously sized buffer means the listener's Receive/
// delete loop is never stalled waiting for the controller to catch up.
const eventsChannelBufferSize = 256

// defaultReconcileInterval is the periodic full-reconciliation cadence used
// when neither --reconcile-interval nor RECONCILE_INTERVAL is set. It is the
// safety net for missed events, vault-side deletions, and in-cluster drift
// (plan.md, data-model.md).
const defaultReconcileInterval = 4 * time.Hour

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(kvsynk8sv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// durationFromEnv parses key as a time.Duration, returning def when the
// variable is unset, empty, or not parseable. Runs before the zap logger is
// configured (it feeds a flag default), so a parse failure is reported to
// stderr rather than through setupLog.
func durationFromEnv(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid %s=%q, falling back to %s: %v\n", key, v, def, err)
		return def
	}
	return d
}

// queueSASWarning is logged when the configured queue URL carries a query
// string. NewQueueSource always authenticates with DefaultAzureCredential
// (constitution V: platform-issued, short-lived credentials only), so a SAS
// token in the URL is not a working auth path here — it is ignored, and it
// only puts a live credential somewhere it does not need to be. This warns
// rather than rejects: dropping the query string outright would change how the
// URL is handed to azqueue.
const queueSASWarning = "Configured queue URL has a query string; it is redacted from logs. " +
	"If it is a SAS token it is not used: this operator always authenticates " +
	"with DefaultAzureCredential. Configure the queue URL without one."

// unparseableQueueURLMessage is what an operator gets instead of the value
// they configured when net/url rejects it. The raw value is deliberately not
// in it: a malformed URL can still be a SAS URL with a typo in it.
//
// It says the operator keeps running because it does: this is a loud
// diagnostic, not a fatal error. See the parse check in main() for why.
const unparseableQueueURLMessage = "Configured queue URL cannot be parsed as a URL " +
	"(the value is not shown: it may contain a SAS token). Fix --queue-url/QUEUE_URL. " +
	"The operator keeps running: queue delivery will not work until this is fixed, " +
	"but every SecretSync still converges through periodic reconciliation."

// queueURLLogKey is the log key both queue-URL log lines use. Named so the
// static check in main_test.go can key off the same constant.
const queueURLLogKey = "queueURL"

// configLogKeyValues builds the key/value pairs of the "Operator
// configuration" startup line.
//
// It exists as a separate function purely so it can be tested: main() has no
// harness, so a log call written inline there is a line no test can observe,
// and the leak this file guards against is exactly a wrong argument at a log
// call. Tests assert that a SAS signature in queueURL never survives into the
// returned slice while the host and path still do.
//
// None of these are secret values: a reconcile interval is operational config,
// and a workload identity client ID is a public identifier, not a credential
// (constitution I only forbids secret *values* — the ones synced into
// Kubernetes Secrets). The queue URL is logged without its query string,
// because that is where a SAS token would sit and a SAS signature is a live
// credential for the queue; see azure.RedactURL.
func configLogKeyValues(queueURL string, reconcileInterval time.Duration, azureClientID string) []any {
	safeQueueURL, _ := azure.RedactURL(queueURL)
	return []any{
		queueURLLogKey, safeQueueURL,
		"reconcileInterval", reconcileInterval,
		"azureClientID", azureClientID,
	}
}

// queueListenerLogKeyValues builds the key/value pairs of the two log lines
// main() emits once it decides to start the queue listener: the SAS warning
// (nil when the configured URL has no query string, meaning "do not warn") and
// the "Queue listener enabled" line.
//
// Same reason as configLogKeyValues for being a function: it puts both the
// warn/don't-warn decision and the arguments of both lines somewhere a test
// can reach, so neither the branch nor its arguments can regress unobserved.
func queueListenerLogKeyValues(queueURL string) (warn []any, enabled []any) {
	safeQueueURL, hadQuery := azure.RedactURL(queueURL)
	if hadQuery {
		warn = []any{queueURLLogKey, safeQueueURL}
	}
	return warn, []any{queueURLLogKey, safeQueueURL}
}

// registerQueueURLFlag registers --queue-url on fs, writing into dst.
//
// The default stays empty on purpose and QUEUE_URL is applied after flag.Parse
// (queueURLFromEnv), for the reason spelled out there. This registration is a
// function rather than a StringVar call inlined in main() so that a test can
// look at it: the leak it prevents is one token wide (passing
// os.Getenv("QUEUE_URL") as the default), main() has no harness, and nothing
// else in the repo reads the flag's DefValue or renders PrintDefaults. See
// TestQueueURLFlagHasNoDefault.
func registerQueueURLFlag(fs *flag.FlagSet, dst *string) {
	fs.StringVar(dst, "queue-url", "",
		"URL of the Azure Storage Queue that receives Key Vault Event Grid "+
			"notifications (env: QUEUE_URL). Optional: without it the operator "+
			"still converges through periodic reconciliation alone.")
}

// queueURLFromEnv returns the QUEUE_URL fallback for --queue-url.
//
// The env value is deliberately NOT used as the flag's default: Go's flag
// package prints a flag's default verbatim in PrintDefaults, so `--help` (or
// any flag parse error, since flag.CommandLine is ExitOnError and calls Usage)
// would write a SAS-bearing QUEUE_URL straight to stderr. Resolving it after
// flag.Parse leaves PrintDefaults with nothing to print. flag.Visit reports
// flags actually supplied on the command line, so an explicit `--queue-url=`
// still means "no queue" and is not silently overridden by a set QUEUE_URL.
func queueURLFromEnv(supplied string) string {
	if flagWasSupplied("queue-url") {
		return supplied
	}
	return os.Getenv("QUEUE_URL")
}

// flagWasSupplied reports whether name was actually given on the command line,
// as opposed to merely having a value.
func flagWasSupplied(name string) bool {
	supplied := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			supplied = true
		}
	})
	return supplied
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
	var queueURL string
	var reconcileInterval time.Duration
	var azureClientID string
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager. "+
			"Accepted for scaffold/manifest compatibility only: this operator runs a single "+
			"replica and never enables leader election in practice (plan.md; Simplicity First) "+
			"— the manager is always started with LeaderElection disabled regardless of this flag.")
	// Registered through a named function, and with no default, on purpose:
	// see registerQueueURLFlag.
	registerQueueURLFlag(flag.CommandLine, &queueURL)
	flag.DurationVar(&reconcileInterval, "reconcile-interval",
		durationFromEnv("RECONCILE_INTERVAL", defaultReconcileInterval),
		"How often to fully reconcile every SecretSync against Key Vault; the "+
			"safety net for missed events, vault-side deletions, and in-cluster "+
			"drift (env: RECONCILE_INTERVAL, default 4h).")
	flag.StringVar(&azureClientID, "azure-client-id", os.Getenv("AZURE_CLIENT_ID"),
		"Client ID of the Microsoft Entra Workload ID used to authenticate to "+
			"Azure (env: AZURE_CLIENT_ID). DefaultAzureCredential already reads "+
			"AZURE_CLIENT_ID from the environment on its own, so this flag mainly "+
			"documents the setting; if set, it is passed through to that same "+
			"environment variable so DefaultAzureCredential still picks it up.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	// Production zap defaults (JSON encoding, info level, sampled): debug
	// logs and dev-style stacktraces are opt-in via --zap-devel, not the
	// baked-in default of a released operator.
	opts := zap.Options{
		Development: false,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	queueURL = queueURLFromEnv(queueURL)

	// Say so loudly, at startup, when net/url cannot parse the configured queue
	// URL. azqueue.NewQueueClient never parses the URL it is given, so a
	// malformed one is accepted at construction and only fails on the first
	// poll — inside http.NewRequestWithContext, which returns a *url.Error
	// containing the raw value. internal/azure redacts that error, but a
	// redacted transport error repeated every idle poll is a poor way to learn
	// that the value in the Deployment has a stray space or a trailing newline
	// in it.
	//
	// This deliberately does NOT exit. The queue path is optional and only
	// buys speed, never correctness (plan.md checkpoint for US2; README's
	// "Queue events not arriving" and queue-health sections): a typo in it must
	// degrade propagation to the periodic reconcile, not crash-loop the
	// operator and take every SecretSync down with it. Startup continues into
	// the normal queue branch below for the same reason — a registered listener
	// keeps the queue health gauges published, and a growing
	// kvsynk8s_queue_consecutive_receive_failures is exactly the documented
	// signal for a broken queue URL.
	if queueURL != "" {
		if _, err := url.Parse(queueURL); err != nil {
			// err is deliberately not passed to the logger: net/url formats a
			// parse failure as `parse "<raw url>": ...`, so logging it would
			// print the very value being withheld.
			setupLog.Error(nil, unparseableQueueURLMessage)
		}
	}

	// DefaultAzureCredential (used by internal/azure's SecretReader/QueueSource,
	// wired in T012/T019) reads AZURE_CLIENT_ID from the environment on its
	// own. Passing it through here lets an operator set the client ID via
	// --azure-client-id as an alternative to setting the env var directly
	// (e.g. a Deployment that only supports command-line args).
	if azureClientID != "" {
		if err := os.Setenv("AZURE_CLIENT_ID", azureClientID); err != nil {
			setupLog.Error(err, "Failed to set AZURE_CLIENT_ID environment variable")
			os.Exit(1)
		}
	}

	configKeyValues := configLogKeyValues(queueURL, reconcileInterval, azureClientID)
	setupLog.Info("Operator configuration", configKeyValues...)

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts
	webhookServerOptions := webhook.Options{
		TLSOpts: webhookTLSOpts,
	}

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	webhookServer := webhook.NewServer(webhookServerOptions)

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.24.1/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.24.1/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		// Owns(&corev1.Secret{}) starts a cluster-wide Secret informer; left
		// unfiltered it would cache every Secret in the cluster, values
		// included. Restrict it to kvsynk8s-managed Secrets only — see
		// controller.ManagedSecretCacheOptions for the conflict-detection
		// consequences this has.
		Cache: controller.ManagedSecretCacheOptions(),
		// Leader election is intentionally always off: this operator runs a
		// single replica and defers leader election until a real HA need
		// appears (plan.md Scale/Scope; constitution III, Simplicity First).
		// enableLeaderElection is parsed above only for --leader-elect
		// scaffold/manifest compatibility and is otherwise ignored.
		LeaderElection:   false,
		LeaderElectionID: "cd7454d8.io",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	secretReader, err := azure.NewSecretReader()
	if err != nil {
		setupLog.Error(err, "Failed to create key vault secret reader")
		os.Exit(1)
	}

	// The queue listener (T017-T019) is optional: US1 must keep working
	// with the queue completely unconfigured (plan.md checkpoint for US2).
	// Without --queue-url/QUEUE_URL, eventsCh stays nil and
	// SetupWithManager adds no WatchesRawSource at all -- the controller's
	// normal reconcile + periodic requeue path is entirely untouched.
	var eventsCh chan event.GenericEvent
	if queueURL != "" {
		sasWarning, listenerEnabled := queueListenerLogKeyValues(queueURL)
		if sasWarning != nil {
			setupLog.Info(queueSASWarning, sasWarning...)
		}
		queueSource, err := azure.NewQueueSource(queueURL)
		if err != nil {
			setupLog.Error(err, "Failed to create storage queue source")
			os.Exit(1)
		}

		eventsCh = make(chan event.GenericEvent, eventsChannelBufferSize)
		listener := events.NewListener(queueSource, mgr.GetClient(), eventsCh)
		if err := mgr.Add(listener); err != nil {
			setupLog.Error(err, "Failed to register queue listener")
			os.Exit(1)
		}
		setupLog.Info("Queue listener enabled", listenerEnabled...)
	} else {
		setupLog.Info("No queue URL configured; relying on periodic reconciliation only")
	}

	if err := (&controller.SecretSyncReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		Reader:            secretReader,
		ReconcileInterval: reconcileInterval,
	}).SetupWithManager(mgr, eventsCh); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "secretsync")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}
