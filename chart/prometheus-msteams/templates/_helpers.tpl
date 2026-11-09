{{/* The chart's name, or nameOverride. */}}
{{- define "promteams.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* The name of every object: fullnameOverride, else the release name, with
     the chart name appended unless the release name already contains it. */}}
{{- define "promteams.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* Labels that select the pods; they never change after the first install. */}}
{{- define "promteams.selectorLabels" -}}
app.kubernetes.io/name: {{ include "promteams.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* Labels on every object. */}}
{{- define "promteams.labels" -}}
{{ include "promteams.selectorLabels" . }}
app.kubernetes.io/version: {{ .Values.image.tag | default .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{/* The path of a template from .Values.templates inside the container. */}}
{{- define "promteams.templatePath" -}}
/etc/templates/{{ . }}.tmpl
{{- end -}}

{{/* Fails unless the template name is defined in .Values.templates.
     Called with a list: (root context, name, where it was used). */}}
{{- define "promteams.checkTemplate" -}}
{{- $root := index . 0 -}}
{{- $name := index . 1 -}}
{{- if not (hasKey $root.Values.templates $name) -}}
{{- fail (printf "%s: template %q is not in templates" (index . 2) $name) -}}
{{- end -}}
{{- end -}}
