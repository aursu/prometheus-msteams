// Package main is the entry point for the prometheus-msteams server.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/hashicorp/go-retryablehttp"
	"github.com/pkg/errors"

	ocprometheus "contrib.go.opencensus.io/exporter/prometheus"
	"github.com/labstack/echo/v4"
	"github.com/prometheus-msteams/prometheus-msteams/pkg/card"
	"github.com/prometheus-msteams/prometheus-msteams/pkg/service"
	"github.com/prometheus-msteams/prometheus-msteams/pkg/transport"
	"github.com/prometheus-msteams/prometheus-msteams/pkg/version"
	stdprometheus "github.com/prometheus/client_golang/prometheus"

	"contrib.go.opencensus.io/exporter/jaeger"
	"go.opencensus.io/plugin/ochttp"
	"go.opencensus.io/stats/view"
	"go.opencensus.io/tag"
	"go.opencensus.io/trace"

	_ "net/http/pprof" //nolint: gosec

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/oklog/run"
	"github.com/peterbourgon/ff"
	"gopkg.in/yaml.v2"
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

//nolint:gocyclo
func main() { //nolint: funlen
	var (
		fs                            = flag.NewFlagSet("prometheus-msteams", flag.ExitOnError)
		promVersion                   = fs.Bool("version", false, "Print the version")
		logFormat                     = fs.String("log-format", "json", "json|fmt")
		debugLogs                     = fs.Bool("debug", false, "Set log level to debug mode.")
		jaegerTrace                   = fs.Bool("jaeger-trace", false, "Send traces to Jaeger.")
		jaegerAgentAddr               = fs.String("jaeger-agent", "localhost:6831", "Jaeger agent endpoint")
		httpAddr                      = fs.String("http-addr", ":2000", "HTTP listen address.")
		requestURI                    = fs.String("teams-request-uri", "", "The default request URI path where Prometheus will post to.")
		teamsWebhookURL               = fs.String("teams-incoming-webhook-url", "", "The default Microsoft Teams webhook connector.")
		templateFile                  = fs.String("template-file", "./default-message-workflow-card.tmpl", "The Adaptive Card template file.")
		escapeUnderscores             = fs.Bool("auto-escape-underscores", true, "Automatically replace all '_' with '\\_' from texts in the alert.")
		configFile                    = fs.String("config-file", "", "The connectors configuration file.")
		httpClientIdleConnTimeout     = fs.Duration("idle-conn-timeout", 90*time.Second, "The HTTP client idle connection timeout duration.")
		httpClientTLSHandshakeTimeout = fs.Duration("tls-handshake-timeout", 30*time.Second, "The HTTP client TLS handshake timeout.")
		httpClientMaxIdleConn         = fs.Int("max-idle-conns", 100, "The HTTP client maximum number of idle connections")
		insecureSkipVerify            = fs.Bool("insecure-skip-verify", false, "Disable validation of the server certificate.")
		retryMax                      = fs.Int("max-retry-count", 3, "The retry maximum for sending requests to the webhook")
		// On by default: a wrong URL stops the program at startup instead of
		// failing every notification later. See validateWebhook for the rules.
		validateWebhookURL = fs.Bool("validate-webhook-url", true, "Check that every webhook URL looks like a Teams Workflow URL (https, Workflow host, .../workflows/.../triggers/manual/paths/invoke, api-version, sp, sv and sig present).")
		// Kept only so that existing command lines with -workflow-webhook still
		// start (an unknown flag stops the program); Workflow is always on now.
		_ = fs.Bool("workflow-webhook", true, "Deprecated, ignored: Workflow webhooks are the only kind.")
	)

	if err := ff.Parse(fs, os.Args[1:], ff.WithEnvVarNoPrefix()); err != nil {
		fmt.Fprint(os.Stderr, err.Error())
		os.Exit(1)
	}

	if *promVersion {
		fmt.Println(version.VERSION)
		os.Exit(0)
	}

	// Logger.
	var logger log.Logger
	{
		switch *logFormat {
		case "json":
			logger = log.NewJSONLogger(log.NewSyncWriter(os.Stdout))
		case "fmt":
			logger = log.NewLogfmtLogger(log.NewSyncWriter(os.Stderr))
		default:
			fmt.Fprintf(os.Stderr, "log-format %s is not valid", *logFormat)
			os.Exit(1)
		}
		if *debugLogs {
			logger = level.NewFilter(logger, level.AllowDebug())
		} else {
			logger = level.NewFilter(logger, level.AllowInfo())
		}
		logger = log.With(logger, "ts", log.DefaultTimestamp, "caller", log.DefaultCaller)
	}

	// Tracer.
	if *jaegerTrace {
		logger.Log("message", "jaeger tracing enabled")

		je, err := jaeger.NewExporter(
			jaeger.Options{
				AgentEndpoint: *jaegerAgentAddr,
				ServiceName:   "prometheus-msteams",
			},
		)
		if err != nil {
			logger.Log("err", err)
			os.Exit(1)
		}

		trace.RegisterExporter(je)
		trace.ApplyConfig(
			trace.Config{
				DefaultSampler: trace.AlwaysSample(),
			},
		)
	}

	// Prepare the Teams config.
	var (
		tc  PromTeamsConfig
		err error
	)

	// Parse the config file if defined.
	if *configFile != "" {
		tc, err = parseTeamsConfigFile(*configFile)
		if err != nil {
			logger.Log("err", err)
			os.Exit(1)
		}
	}

	// Templated card defaultConverter setup.
	var defaultConverter card.Converter
	{
		tmpl, err := card.ParseTemplateFile(*templateFile)
		if err != nil {
			logger.Log("err", err)
		}
		defaultConverter = card.NewTemplatedCardCreator(tmpl, *escapeUnderscores)
		defaultConverter = card.NewCreatorLoggingMiddleware(
			log.With(
				logger,
				"template_file", *templateFile,
				"escaped_underscores", *escapeUnderscores,
			),
			defaultConverter,
		)
	}

	// Teams HTTP client setup.
	retryClient := retryablehttp.NewClient()
	if !*debugLogs {
		retryClient.Logger = nil
	}
	retryClient.RetryMax = *retryMax
	retryClient.HTTPClient = &http.Client{
		Transport: &ochttp.Transport{
			Base: &http.Transport{
				Proxy: http.ProxyFromEnvironment,
				DialContext: (&net.Dialer{
					Timeout:   30 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				MaxIdleConns:          *httpClientMaxIdleConn,
				IdleConnTimeout:       *httpClientIdleConnTimeout,
				TLSHandshakeTimeout:   *httpClientTLSHandshakeTimeout,
				ExpectContinueTimeout: 1 * time.Second,
				TLSClientConfig:       &tls.Config{InsecureSkipVerify: *insecureSkipVerify}, //nolint: gosec
			},
		},
	}

	httpClient := retryClient.StandardClient()

	var routes []transport.Route
	var dRoutes []transport.DynamicRoute

	// Connectors from flags.
	if len(*requestURI) > 0 && len(*teamsWebhookURL) > 0 {
		tc.Connectors = append(
			tc.Connectors,
			map[string]string{
				*requestURI: *teamsWebhookURL,
			},
		)
	}

	{ // dynamic uri handler: webhook uri is retrieved from request.URL
		var r transport.DynamicRoute
		r.RequestPath = "/_dynamicwebhook/*"
		r.ServiceGenerator = func(c echo.Context) (service.Service, error) {
			webhook, err := extractWebhookFromRequest(c.Request(), "/_dynamicwebhook/")
			if err != nil {
				err = errors.Wrapf(err, "webhook extraction failed for /_dynamicwebhook/")
				logger.Log("err", err)
				return nil, err
			}

			err = validateWebhook(webhook)
			if *validateWebhookURL && err != nil {
				err = errors.Wrapf(err, "webhook validation failed for /_dynamicwebhook/")
				logger.Log("err", err)
				return nil, err
			}

			var s service.Service
			s = service.NewSimpleService(defaultConverter, httpClient, webhook)
			s = service.NewLoggingService(logger, s)
			return s, nil
		}
		dRoutes = append(dRoutes, r)
	}

	// Connectors from config file.
	for _, c := range tc.Connectors {
		for uri, webhook := range c {
			err := validateWebhook(webhook)
			if *validateWebhookURL && err != nil {
				logger.Log("err", err)
				os.Exit(1)
			}

			var r transport.Route
			r.RequestPath = uri
			r.Service = service.NewSimpleService(defaultConverter, httpClient, webhook)
			r.Service = service.NewLoggingService(logger, r.Service)
			routes = append(routes, r)
		}
	}

	// Connectors with custom template files.
	for _, c := range tc.ConnectorsWithCustomTemplates {
		if len(c.RequestPath) == 0 {
			logger.Log("err", "one of the 'templated_connectors' is missing a 'request_path'")
			os.Exit(1)
		}
		if len(c.WebhookURL) == 0 {
			logger.Log(
				"err",
				fmt.Sprintf("The webhook_url is required for request_path '%s'", c.RequestPath),
			)
			os.Exit(1)
		}
		err := validateWebhook(c.WebhookURL)
		if *validateWebhookURL && err != nil {
			logger.Log("err", err)
			os.Exit(1)
		}
		if len(c.TemplateFile) == 0 {
			logger.Log(
				"err",
				fmt.Sprintf("The template_file is required for request_path '%s'", c.RequestPath),
			)
			os.Exit(1)
		}

		var converter card.Converter
		tmpl, err := card.ParseTemplateFile(c.TemplateFile)
		if err != nil {
			logger.Log("err", err)
			os.Exit(1)
		}

		// converter = card.NewTemplatedCardCreator(tmpl, c.EscapeUnderscores, c.DisableGrouping)
		converter = card.NewTemplatedCardCreator(tmpl, c.EscapeUnderscores)
		converter = card.NewCreatorLoggingMiddleware(
			log.With(
				logger,
				"template_file", c.TemplateFile,
				"escaped_underscores", c.EscapeUnderscores,
			),
			converter,
		)

		var r transport.Route
		r.RequestPath = c.RequestPath
		r.Service = service.NewSimpleService(converter, httpClient, c.WebhookURL)
		r.Service = service.NewLoggingService(logger, r.Service)
		routes = append(routes, r)
	}

	if err := checkDuplicateRequestPath(routes); err != nil {
		logger.Log("err", err)
		os.Exit(1)
	}

	pe, err := ocprometheus.NewExporter(
		ocprometheus.Options{
			Registry: stdprometheus.DefaultRegisterer.(*stdprometheus.Registry),
		},
	)
	if err != nil {
		logger.Log("err", err)
		os.Exit(1)
	}
	if err := view.Register(ocviews()...); err != nil {
		logger.Log("err", err)
		os.Exit(1)
	}

	// Prometheus msteams HTTP handler setup.
	var handler *echo.Echo
	{
		// Main app.
		handler = transport.NewServer(logger, routes, dRoutes)
		// Prometheus metrics.
		handler.GET("/metrics", echo.WrapHandler(pe))
		// Pprof.
		handler.GET("/debug/pprof/*", echo.WrapHandler(http.DefaultServeMux))
		// Config.
		handler.GET("/config", func(c echo.Context) error {
			return c.JSON(200, tc.Connectors)
		})
	}

	var g run.Group
	{
		srv := http.Server{
			Addr:              *httpAddr,
			Handler:           handler,
			ReadHeaderTimeout: 30 * time.Second,
		}
		g.Add(
			func() error {
				logger.Log(
					"listen_http_addr", *httpAddr,
					"version", version.VERSION,
					"commit", version.COMMIT,
					"branch", version.BRANCH,
					"build_date", version.BUILDDATE,
				)
				return srv.ListenAndServe()
			},
			func(error) {
				if err != http.ErrServerClosed {
					if err := srv.Shutdown(context.Background()); err != nil {
						logger.Log("err", err)
					}
				}
			},
		)
	}
	{
		g.Add(run.SignalHandler(context.Background(), syscall.SIGINT, syscall.SIGTERM))
	}
	logger.Log("exit", g.Run())
}

func ocviews() []*view.View {
	clientKeys := []tag.Key{
		ochttp.KeyClientMethod, ochttp.KeyClientStatus, ochttp.KeyClientHost, ochttp.KeyClientPath,
	}
	serverKeys := []tag.Key{
		ochttp.StatusCode, ochttp.Method, ochttp.Path,
	}
	return []*view.View{
		// HTTP client metrics.
		{
			Name:        "http/client/sent_bytes",
			Measure:     ochttp.ClientSentBytes,
			Aggregation: view.Distribution(1024, 2048, 4096, 16384, 65536, 262144, 1048576, 4194304),
			Description: "Total bytes sent in request body (not including headers), by HTTP method and response status",
			TagKeys:     clientKeys,
		},
		{
			Name:        "http/client/received_bytes",
			Measure:     ochttp.ClientReceivedBytes,
			Aggregation: view.Distribution(1024, 2048, 4096, 16384, 65536, 262144, 1048576, 4194304),
			Description: "Total bytes received in response bodies (not including headers but including error responses with bodies), by HTTP method and response status",
			TagKeys:     clientKeys,
		},
		{
			Name:        "http/client/roundtrip_latency",
			Measure:     ochttp.ClientRoundtripLatency,
			Aggregation: view.Distribution(1, 2, 3, 4, 5, 6, 8, 10, 13, 16, 20, 25, 30),
			Description: "End-to-end latency, by HTTP method and response status",
			TagKeys:     clientKeys,
		},
		{
			Name:        "http/client/completed_count",
			Measure:     ochttp.ClientRoundtripLatency,
			Aggregation: view.Count(),
			Description: "Count of completed requests, by HTTP method and response status",
			TagKeys:     clientKeys,
		},
		// HTTP server metrics.
		{
			Name:        "http/server/request_count",
			Description: "Count of HTTP requests started",
			Measure:     ochttp.ServerRequestCount,
			Aggregation: view.Count(),
			TagKeys:     serverKeys,
		},
		{
			Name:        "http/server/request_bytes",
			Description: "Size distribution of HTTP request body",
			Measure:     ochttp.ServerRequestBytes,
			Aggregation: view.Distribution(1024, 2048, 4096, 16384, 65536, 262144, 1048576, 4194304),
			TagKeys:     serverKeys,
		},
		{
			Name:        "http/server/response_bytes",
			Description: "Size distribution of HTTP response body",
			Measure:     ochttp.ServerResponseBytes,
			Aggregation: view.Distribution(1024, 2048, 4096, 16384, 65536, 262144, 1048576, 4194304),
			TagKeys:     serverKeys,
		},
		{
			Name:        "http/server/latency",
			Description: "Latency distribution of HTTP requests",
			Measure:     ochttp.ServerLatency,
			Aggregation: view.Distribution(1, 2, 3, 4, 5, 6, 8, 10, 13, 16, 20, 25, 30),
			TagKeys:     serverKeys,
		},
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

const bearerType = "webhook"

func extractWebhookFromRequest(request *http.Request, requestPathPrefix string) (string, error) {
	var pathAndQuery string

	// Query contains mandatory "api-version" for "worflow"
	if len(request.URL.Query()) > 0 {
		pathAndQuery = request.URL.Path + "?" + request.URL.RawQuery
	} else {
		pathAndQuery = request.URL.Path
	}

	pathAndQuery = strings.TrimPrefix(pathAndQuery, requestPathPrefix)
	if pathAndQuery != "" {
		return fmt.Sprintf("https://%s", pathAndQuery), nil
	}
	authHeader := request.Header.Get("Authorization")
	if authHeader == "" {
		return "", fmt.Errorf("neither webhook url nor bearer authorization present")
	}

	if !strings.HasPrefix(authHeader, bearerType) {
		return "", fmt.Errorf("invalid bearer on authorization")
	}
	pathAndQuery = strings.TrimPrefix(authHeader, fmt.Sprintf("%s ", bearerType))
	return fmt.Sprintf("https://%s", pathAndQuery), nil
}
