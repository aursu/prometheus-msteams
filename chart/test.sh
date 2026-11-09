#!/bin/sh
# Tests the Helm chart: lint, what it renders, and the errors it gives for
# a configuration it cannot serve. Needs helm. Run from the repository root:
#   sh chart/test.sh
set -eu

CHART=chart/prometheus-msteams
FULL=$CHART/ci/full-values.yaml
failures=0

fail() {
	echo "FAIL: $*"
	failures=$((failures + 1))
}

# render <args...>: helm template with the given values; output on stdout.
render() {
	helm template promteams "$CHART" "$@" 2>&1
}

# expect_contains <text> <description>: the last rendering holds text.
expect_contains() {
	printf '%s' "$out" | grep -qF -- "$1" || fail "$2: no \"$1\" in the output"
}

# expect_error <message part> <description> <helm args...>
expect_error() {
	want=$1 desc=$2
	shift 2
	if got=$(render "$@"); then
		fail "$desc: rendered, want an error"
	elif ! printf '%s' "$got" | grep -qF -- "$want"; then
		fail "$desc: error does not say \"$want\": $got"
	fi
}

helm lint --strict "$CHART" -f "$FULL" >/dev/null || fail "helm lint"

out=$(render -f "$FULL")
expect_contains 'webhook_url_file: "/etc/teams/critical"' "URL file of a channel"
expect_contains 'template_file: "/etc/templates/compact.tmpl"' "channel template"
expect_contains 'escape_underscores: true' "escapeUnderscores"
expect_contains 'compact.tmpl: |' "templates ConfigMap"
expect_contains '- -template-file=/etc/templates/compact.tmpl' "defaultTemplate"
expect_contains '- -max-card-size=20000' "extraArgs"
expect_contains 'secretName: teams-urls' "Secret volume"
expect_contains 'defaultMode: 0440' "Secret file mode"
expect_contains 'fsGroup: 65532' "fsGroup"
expect_contains 'path: /-/ready' "readiness probe"
expect_contains 'kind: ServiceMonitor' "ServiceMonitor"
expect_contains 'image: "ghcr.io/aursu/prometheus-msteams:v' "image tag from appVersion"
# Not a single URL or secret value comes from the chart.
printf '%s' "$out" | grep -q 'webhook_url:' && fail "inline webhook_url rendered"

# The smallest configuration: no own templates, no ServiceMonitor.
out=$(render --set urlSecret.name=s --set 'channels[0].path=/a' --set 'channels[0].urlKey=a')
printf '%s' "$out" | grep -q -- '-template-file' && fail "minimal: -template-file without defaultTemplate"
printf '%s' "$out" | grep -q 'name: templates' && fail "minimal: templates volume without templates"
printf '%s' "$out" | grep -q 'kind: ServiceMonitor' && fail "minimal: ServiceMonitor while disabled"

expect_error "at least one channel" "no channels" --set urlSecret.name=s
expect_error "urlSecret.name" "no Secret" --set 'channels[0].path=/a' --set 'channels[0].urlKey=a'
expect_error "urlKey is required" "channel without urlKey" --set urlSecret.name=s --set 'channels[0].path=/a'
expect_error 'template "nope" is not in templates' "unknown channel template" \
	--set urlSecret.name=s --set 'channels[0].path=/a' --set 'channels[0].urlKey=a' --set 'channels[0].template=nope'
expect_error 'defaultTemplate: template "nope"' "unknown defaultTemplate" \
	--set urlSecret.name=s --set 'channels[0].path=/a' --set 'channels[0].urlKey=a' --set defaultTemplate=nope

if [ "$failures" -gt 0 ]; then
	echo "$failures chart test(s) failed"
	exit 1
fi
echo "chart tests passed"
