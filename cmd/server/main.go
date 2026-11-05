// Package main is the entry point for the prometheus-msteams server.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/hashicorp/go-retryablehttp"
	"github.com/oklog/run"
	"github.com/peterbourgon/ff/v3"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.yaml.in/yaml/v3"

	"github.com/aursu/prometheus-msteams/pkg/card"
	"github.com/aursu/prometheus-msteams/pkg/service"
	"github.com/aursu/prometheus-msteams/pkg/transport"
	"github.com/aursu/prometheus-msteams/pkg/version"
)

// PromTeamsConfig is the struct representation of the config file.
type PromTeamsConfig struct {
	// Connectors
	// The key is the request path for Prometheus to post to.
	// The value is the Teams webhook url.
	Connectors                    []map[string]string           `yaml:"connectors"`
	ConnectorsWithCustomTemplates []ConnectorWithCustomTemplate `yaml:"connectors_with_custom_templates"`
}

// ConnectorWithCustomTemplate .
type ConnectorWithCustomTemplate struct {
	RequestPath       string `yaml:"request_path"`
	TemplateFile      string `yaml:"template_file"`
	WebhookURL        string `yaml:"webhook_url"`
	EscapeUnderscores bool   `yaml:"escape_underscores"`
}

// parseFlags reads the command line into fs. Every flag can also be set by an
// environment variable: the flag name in upper case with "-" turned into "_"
// (-teams-timeout -> TEAMS_TIMEOUT). A flag on the command line wins over
// the variable.
func parseFlags(fs *flag.FlagSet, args []string) error {
	return ff.Parse(fs, args, ff.WithEnvVars())
}

func parseTeamsConfigFile(f string) (PromTeamsConfig, error) {
	b, err := os.ReadFile(f) //nolint:gosec
	if err != nil {
		return PromTeamsConfig{}, err
	}
	var tc PromTeamsConfig
	if err = yaml.Unmarshal(b, &tc); err != nil {
		return PromTeamsConfig{}, err
	}
	return tc, nil
}

// The parts of a Teams Workflow URL that stay the same across tenants,
// regions and URL generations. Everything else varies and is not checked: the
// tenant/environment prefix of the host, the region, an explicit ":443",
// extra path segments such as "/powerautomate/automations/direct/cu/19", the
// workflow id and the values of the query parameters.
//
// Example (current generation):
//
//	https://<env>.<region>.environment.api.powerplatform.com:443/powerautomate/automations/direct/cu/19/workflows/<id>/triggers/manual/paths/invoke?api-version=1&sp=...&sv=1.0&sig=...
//
// (A "var (...)" block declares several package-level variables at once.)
var (
	// The host must end with one of these. logic.azure.com is the host of
	// Workflows created before Microsoft moved them to Power Platform.
	workflowHostSuffixes = []string{
		".environment.api.powerplatform.com",
		".logic.azure.com",
	}
	// The path must contain workflowPathMarker and end with workflowPathSuffix.
	workflowPathMarker = "/workflows/"
	workflowPathSuffix = "/triggers/manual/paths/invoke"
	// Each of these query parameters must be present and non-empty.
	// "api-version" is required by the endpoint itself. "sp", "sv" and "sig"
	// are the Shared Access Signature (SAS): permissions, signature version
	// and the signature over them - the secret part of the URL and the only
	// way this program can authenticate to the Workflow.
	workflowQueryParams = []string{"api-version", "sp", "sv", "sig"}
)

// validateWebhook checks that u looks like a Teams Workflow URL, by the
// constant parts listed above. It returns nil when the URL is fine, otherwise
// an error saying which part is wrong. (Go has no exceptions: a function
// returns an error value, and the caller checks it for nil.)
//
// The error never contains the URL itself: the query holds the signature,
// and errors end up in logs.
func validateWebhook(u string) error {
	// url.Parse splits the URL into scheme, host, path and query. Its own
	// error text would quote the whole URL, so it is not passed on.
	parsed, err := url.Parse(u)
	if err != nil {
		return errors.New("the webhook_url is not a valid URL")
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("the webhook_url must use https, not %q", parsed.Scheme)
	}

	// Hostname() is the host without the port, so ":443" does not matter.
	host := strings.ToLower(parsed.Hostname())
	if !hasAnySuffix(host, workflowHostSuffixes) {
		return fmt.Errorf("the webhook_url host %q is not a Teams Workflow host (*%s)",
			host, strings.Join(workflowHostSuffixes, ", *"))
	}

	if !strings.Contains(parsed.Path, workflowPathMarker) || !strings.HasSuffix(parsed.Path, workflowPathSuffix) {
		return fmt.Errorf("the webhook_url path must contain %q and end with %q",
			workflowPathMarker, workflowPathSuffix)
	}

	query := parsed.Query()
	for _, name := range workflowQueryParams {
		if query.Get(name) != "" {
			continue
		}
		// A Workflow whose trigger is restricted to signed-in users ("Who can
		// trigger the flow?" set to a tenant option instead of "Anyone") is
		// called with an Entra ID token instead of a signature. This program
		// cannot obtain such tokens, so the URL is unusable, not just odd.
		if name == "sig" {
			return errors.New("the webhook_url has no \"sig\" query parameter: the Workflow " +
				"probably requires signed-in callers; set its trigger's \"Who can trigger the flow?\" to \"Anyone\"")
		}
		return fmt.Errorf("the webhook_url has no %q query parameter", name)
	}
	return nil
}

// hasAnySuffix reports whether s ends with one of the suffixes.
func hasAnySuffix(s string, suffixes []string) bool {
	for _, suffix := range suffixes {
		if strings.HasSuffix(s, suffix) {
			return true
		}
	}
	return false
}

// teamsClientOptions are the settings of the HTTP client that sends the cards
// to Teams, taken from the command-line flags.
type teamsClientOptions struct {
	retryMax            int
	maxIdleConns        int
	idleConnTimeout     time.Duration
	tlsHandshakeTimeout time.Duration
	insecureSkipVerify  bool
}

// newTeamsClient builds the HTTP client that sends the cards to Teams: a
// retry client (hashicorp/go-retryablehttp) behind the standard *http.Client
// interface. It sets no overall timeout; each delivery is limited by
// -teams-timeout through its context (see service.NewSimpleService).
func newTeamsClient(o teamsClientOptions) *http.Client {
	retryClient := retryablehttp.NewClient()
	// The retry client's own logger prints every request URL, signature
	// included (it masks only passwords), so it stays off even with -debug.
	retryClient.Logger = nil
	// When the retries are used up, hand back the last response or error as
	// it is. The default instead returns a new error "POST <full URL> giving
	// up after N attempt(s)": the status code is lost and the URL leaks.
	retryClient.ErrorHandler = retryablehttp.PassthroughErrorHandler
	retryClient.RetryMax = o.retryMax
	base := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          o.maxIdleConns,
		IdleConnTimeout:       o.idleConnTimeout,
		TLSHandshakeTimeout:   o.tlsHandshakeTimeout,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: o.insecureSkipVerify}, //nolint: gosec
	}
	// The metrics sit below the retry client, so every attempt is counted
	// with the status Teams answered. A request that got no answer at all
	// (connection refused, timeout) has no status and is not counted.
	retryClient.HTTPClient = &http.Client{
		Transport: promhttp.InstrumentRoundTripperCounter(teamsRequestsTotal,
			promhttp.InstrumentRoundTripperDuration(teamsRequestDuration, base)),
	}
	return retryClient.StandardClient()
}

// Metrics of the requests to Teams, served on /metrics. The "code" label is
// Teams' answer: 202 accepted; 400, 404, 413 refused; 429 throttled; 5xx.
var (
	teamsRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "prometheus_msteams_teams_requests_total",
		Help: "Requests to Teams Workflows, every retry included, by answered status code.",
	}, []string{"code"})
	teamsRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "prometheus_msteams_teams_request_duration_seconds",
		Help:    "Time until Teams answered a request.",
		Buckets: prometheus.DefBuckets,
	}, []string{})
)

//nolint:gocyclo
func main() { //nolint: funlen
	var (
		fs              = flag.NewFlagSet("prometheus-msteams", flag.ExitOnError)
		promVersion     = fs.Bool("version", false, "Print the version")
		logFormat       = fs.String("log-format", "json", "json|fmt")
		debugLogs       = fs.Bool("debug", false, "Set log level to debug mode.")
		httpAddr        = fs.String("http-addr", ":2000", "HTTP listen address.")
		requestURI      = fs.String("teams-request-uri", "", "The default request URI path where Prometheus will post to.")
		teamsWebhookURL = fs.String("teams-incoming-webhook-url", "", "The default Microsoft Teams webhook connector.")
		templateFile    = fs.String("template-file", "./default-message-workflow-card.tmpl", "The Adaptive Card template file.")
		// Off by default: it rewrites every label and annotation value
		// before rendering, which also breaks links built from them. The
		// templates escape each value where they use it (mdEscape, toJson).
		escapeUnderscores             = fs.Bool("auto-escape-underscores", false, "Before rendering, JSON-escape every label and annotation value and replace '_' with '\\_'. Only for templates that do not escape values themselves.")
		configFile                    = fs.String("config-file", "", "The connectors configuration file.")
		httpClientIdleConnTimeout     = fs.Duration("idle-conn-timeout", 90*time.Second, "The HTTP client idle connection timeout duration.")
		httpClientTLSHandshakeTimeout = fs.Duration("tls-handshake-timeout", 30*time.Second, "The HTTP client TLS handshake timeout.")
		httpClientMaxIdleConn         = fs.Int("max-idle-conns", 100, "The HTTP client maximum number of idle connections")
		insecureSkipVerify            = fs.Bool("insecure-skip-verify", false, "Disable validation of the server certificate.")
		// 0 by default: a failed delivery is answered at once, and
		// Alertmanager retries it (it retries 5xx answers, see statusFor in
		// package transport). Retries here would keep Alertmanager waiting.
		retryMax = fs.Int("max-retry-count", 0, "How often this service itself retries a failed delivery to Teams. 0 leaves retries to Alertmanager.")
		// One delivery, retries included, may take this long; then
		// Alertmanager is answered 504 and retries later.
		teamsTimeout = fs.Duration("teams-timeout", 30*time.Second, "How long one delivery to Teams may take, retries included. 0 means no limit.")
		// On by default: a wrong URL stops the program at startup instead of
		// failing every notification later. See validateWebhook for the rules.
		validateWebhookURL = fs.Bool("validate-webhook-url", true, "Check that every webhook URL looks like a Teams Workflow URL (https, Workflow host, .../workflows/.../triggers/manual/paths/invoke, api-version, sp, sv and sig present).")
		// Kept only so that existing command lines with -workflow-webhook still
		// start (an unknown flag stops the program); Workflow is always on now.
		_ = fs.Bool("workflow-webhook", true, "Deprecated, ignored: Workflow webhooks are the only kind.")
		// The same for the tracing flags: tracing to Jaeger was removed with
		// the OpenCensus library it was built on.
		_ = fs.Bool("jaeger-trace", false, "Deprecated, ignored: tracing was removed.")
		_ = fs.String("jaeger-agent", "", "Deprecated, ignored: tracing was removed.")
	)

	if err := parseFlags(fs, os.Args[1:]); err != nil {
		fmt.Fprint(os.Stderr, err.Error())
		os.Exit(1)
	}

	if *promVersion {
		fmt.Println(version.VERSION)
		os.Exit(0)
	}

	logger, err := newLogger(os.Stderr, *logFormat, *debugLogs)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// fatal logs an error and stops the program; used for configuration
	// errors at startup. (A function value assigned to a variable: Go's
	// local helper functions are written this way.)
	fatal := func(msg string, args ...any) {
		logger.Error(msg, args...)
		os.Exit(1)
	}

	// Prepare the Teams config.
	var tc PromTeamsConfig

	// Parse the config file if defined.
	if *configFile != "" {
		tc, err = parseTeamsConfigFile(*configFile)
		if err != nil {
			fatal("cannot read the configuration file", "err", err)
		}
	}

	// Templated card defaultConverter setup.
	var defaultConverter card.Converter
	{
		tmpl, err := card.ParseTemplateFile(*templateFile)
		if err != nil {
			fatal("cannot read the template", "template_file", *templateFile, "err", err)
		}
		defaultConverter = card.NewTemplatedCardCreator(tmpl, *escapeUnderscores)
		defaultConverter = card.NewCreatorLoggingMiddleware(
			logger.With(
				"template_file", *templateFile,
				"escaped_underscores", *escapeUnderscores,
			),
			defaultConverter,
		)
	}

	httpClient := newTeamsClient(teamsClientOptions{
		retryMax:            *retryMax,
		maxIdleConns:        *httpClientMaxIdleConn,
		idleConnTimeout:     *httpClientIdleConnTimeout,
		tlsHandshakeTimeout: *httpClientTLSHandshakeTimeout,
		insecureSkipVerify:  *insecureSkipVerify,
	})

	var routes []transport.Route

	// Connectors from flags.
	if len(*requestURI) > 0 && len(*teamsWebhookURL) > 0 {
		tc.Connectors = append(
			tc.Connectors,
			map[string]string{
				*requestURI: *teamsWebhookURL,
			},
		)
	}

	// Connectors from config file. Every Teams URL comes from the
	// configuration; there is no route that takes the URL from the request.
	for _, c := range tc.Connectors {
		for uri, webhook := range c {
			err := validateWebhook(webhook)
			if *validateWebhookURL && err != nil {
				fatal("invalid webhook URL", "request_path", uri, "err", err)
			}

			var r transport.Route
			r.RequestPath = uri
			r.Service = service.NewSimpleService(defaultConverter, httpClient, webhook, *teamsTimeout)
			r.Service = service.NewLoggingService(logger, r.Service)
			routes = append(routes, r)
		}
	}

	// Connectors with custom template files.
	for _, c := range tc.ConnectorsWithCustomTemplates {
		if len(c.RequestPath) == 0 {
			fatal("one of the connectors_with_custom_templates has no request_path")
		}
		if len(c.WebhookURL) == 0 {
			fatal("webhook_url is required", "request_path", c.RequestPath)
		}
		err := validateWebhook(c.WebhookURL)
		if *validateWebhookURL && err != nil {
			fatal("invalid webhook URL", "request_path", c.RequestPath, "err", err)
		}
		if len(c.TemplateFile) == 0 {
			fatal("template_file is required", "request_path", c.RequestPath)
		}

		var converter card.Converter
		tmpl, err := card.ParseTemplateFile(c.TemplateFile)
		if err != nil {
			fatal("cannot read the template", "template_file", c.TemplateFile, "err", err)
		}

		converter = card.NewTemplatedCardCreator(tmpl, c.EscapeUnderscores)
		converter = card.NewCreatorLoggingMiddleware(
			logger.With(
				"template_file", c.TemplateFile,
				"escaped_underscores", c.EscapeUnderscores,
			),
			converter,
		)

		var r transport.Route
		r.RequestPath = c.RequestPath
		r.Service = service.NewSimpleService(converter, httpClient, c.WebhookURL, *teamsTimeout)
		r.Service = service.NewLoggingService(logger, r.Service)
		routes = append(routes, r)
	}

	if err := checkDuplicateRequestPath(routes); err != nil {
		fatal("invalid configuration", "err", err)
	}

	// Prometheus msteams HTTP handler setup: the webhook routes and health
	// checks (transport.NewServer) plus the metrics. Nothing else is served:
	// the profiler (/debug/pprof) and /config, which listed the Teams URLs
	// with their signatures, are gone.
	handler, err := transport.NewServer(logger, routes)
	if err != nil {
		fatal("invalid configuration", "err", err)
	}
	// Prometheus metrics: those defined with promauto in this program, plus
	// the Go runtime and process metrics the client library adds itself.
	handler.Handle("GET /metrics", promhttp.Handler())

	var g run.Group
	{
		srv := http.Server{
			Addr:              *httpAddr,
			Handler:           handler,
			ReadHeaderTimeout: 30 * time.Second,
		}
		g.Add(
			func() error {
				logger.Info("listening",
					"listen_http_addr", *httpAddr,
					"version", version.VERSION,
					"commit", version.COMMIT,
					"branch", version.BRANCH,
					"build_date", version.BUILDDATE,
				)
				return srv.ListenAndServe()
			},
			func(error) {
				if err := srv.Shutdown(context.Background()); err != nil {
					logger.Error("shutdown failed", "err", err)
				}
			},
		)
	}
	{
		g.Add(run.SignalHandler(context.Background(), syscall.SIGINT, syscall.SIGTERM))
	}
	logger.Info("exit", "reason", g.Run())
}

// newLogger creates the program's logger (log/slog from the standard
// library). format is "json" (one JSON object per line) or "fmt" (key=value
// text); debug also lets Debug messages through, which include every alert
// and card in full. Each line carries the time, the level and the source
// file and line it was logged from.
func newLogger(w io.Writer, format string, debug bool) (*slog.Logger, error) {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	opts := &slog.HandlerOptions{Level: level, AddSource: true}
	switch format {
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	case "fmt":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	default:
		return nil, fmt.Errorf("-log-format %q is not valid, want json or fmt", format)
	}
}

func checkDuplicateRequestPath(routes []transport.Route) error {
	added := map[string]bool{}
	for _, r := range routes {
		if _, ok := added[r.RequestPath]; ok {
			return fmt.Errorf("found duplicate use of request path '%s'", r.RequestPath)
		}
		added[r.RequestPath] = true
	}
	return nil
}
