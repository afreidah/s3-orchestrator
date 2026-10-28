{{/*
-------------------------------------------------------------------------------
Helm Template - Helpers

Author: Alex Freidah

Names and labels shared by every template, the image reference, the config
the release runs, and the preflight checks that stop an install the
orchestrator cannot run correctly.
-------------------------------------------------------------------------------
*/}}

{{/* --- Names and labels --- */}}
{{- define "s3-orchestrator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "s3-orchestrator.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "s3-orchestrator.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "s3-orchestrator.selectorLabels" . }}
app.kubernetes.io/version: {{ include "s3-orchestrator.imageTag" . | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "s3-orchestrator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "s3-orchestrator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "s3-orchestrator.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "s3-orchestrator.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/* --- Image: a digest pins it, otherwise the tag, otherwise the chart's release --- */}}
{{- define "s3-orchestrator.imageTag" -}}
{{- default .Chart.AppVersion .Values.image.tag | trunc 63 }}
{{- end }}

{{- define "s3-orchestrator.image" -}}
{{- if .Values.image.digest }}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest }}
{{- else }}
{{- printf "%s:%s" .Values.image.repository (include "s3-orchestrator.imageTag" .) }}
{{- end }}
{{- end }}

{{/* --- The config the release runs, as YAML: configFile whole, or config --- */}}
{{- define "s3-orchestrator.config" -}}
{{- if .Values.configFile }}
{{- .Values.configFile }}
{{- else }}
{{- toYaml .Values.config }}
{{- end }}
{{- end }}

{{/* --- Preflight: refuse what the orchestrator cannot run correctly --- */}}
{{- define "s3-orchestrator.preflight" -}}
{{- $config := include "s3-orchestrator.config" . | fromYaml }}
{{- if and (not .Values.image.digest) (not (include "s3-orchestrator.imageTag" .)) }}
{{- fail "image.tag is required: a chart installed from a checkout has no appVersion to default it to" }}
{{- end }}
{{- if eq (dig "database" "driver" "postgres" $config) "sqlite" }}
{{- fail "database.driver sqlite cannot run on this chart: the root filesystem is read-only and a pod keeps nothing across a restart; use PostgreSQL" }}
{{- end }}
{{- if and (or (gt (int .Values.replicaCount) 1) .Values.autoscaling.enabled) (not (hasKey $config "redis")) }}
{{- fail "more than one replica requires redis in the config: without it each replica enforces usage limits against its own counters" }}
{{- end }}
{{- end }}
