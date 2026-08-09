{{/*
Standard name/label helpers, the same shape virtually every Helm chart
carries (this repeats the well-known pattern; it is not a dependency on any
external chart, "bitnami" or otherwise — see values.yaml's postgresql
comment for why that distinction matters here).
*/}}

{{- define "culture-nodes.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "culture-nodes.fullname" -}}
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

{{- define "culture-nodes.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "culture-nodes.labels" -}}
helm.sh/chart: {{ include "culture-nodes.chart" . }}
{{ include "culture-nodes.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "culture-nodes.selectorLabels" -}}
app.kubernetes.io/name: {{ include "culture-nodes.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
culture-nodes.image renders the one image reference every role Deployment
and the migration Job share. digest wins over tag when set — see
values.yaml's image.digest comment for why: it is what release.yml's own
smoke job verified, a mutable tag is not.
*/}}
{{- define "culture-nodes.image" -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository (.Values.image.tag | default .Chart.AppVersion) -}}
{{- end -}}
{{- end -}}

{{/*
culture-nodes.postgresPassword resolves the in-chart Postgres password:
values.postgresql.auth.password when set, otherwise whatever the existing
release's Secret already holds (so `helm upgrade` never silently rotates
the password out from under a running database), otherwise a fresh random
value. `helm template` (no live cluster) always takes the lookup's "not
found" branch, which is why manifest tests that need a stable rendered
value pass --set postgresql.auth.password explicitly.

CALL THIS AT MOST ONCE PER RENDER, from templates/secret-database.yaml
only, and thread the result to every other place that needs it (see that
file's own comment). The random fallback (randAlphaNum) is re-evaluated
fresh on every `include`, so calling this helper from two different
template files would silently mint two different passwords in the same
`helm install` — this chart shipped that exact bug once, caught only by a
real `helm install` against a live cluster, not by `helm template`.
*/}}
{{- define "culture-nodes.postgresPassword" -}}
{{- if .Values.postgresql.auth.password -}}
{{- .Values.postgresql.auth.password -}}
{{- else -}}
{{- /*
The -db Secret (templates/secret-database.yaml), not a "-postgres" one --
that's the ONLY place POSTGRES_PASSWORD is ever written (see this helper's
own "call at most once" warning above). Looking up the wrong secret name
here always misses, which always falls through to a fresh random value on
every single `helm upgrade` -- silently breaking a running Postgres's
password every time. This chart shipped that exact bug once too, right
after merging what used to be a separate postgres-only Secret into this
one and forgetting to update the name here; caught only by a second real
`helm upgrade` against a live cluster (the first `helm install` cannot
reveal it -- there is nothing to look up yet on a fresh install).
*/ -}}
{{- $secretName := printf "%s-db" (include "culture-nodes.fullname" .) -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace $secretName -}}
{{- if $existing -}}
{{- index $existing.data "POSTGRES_PASSWORD" | b64dec -}}
{{- else -}}
{{- randAlphaNum 24 -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
culture-nodes.callbackTokenSecret follows the identical
set-value/existing-Secret/random precedence as
culture-nodes.postgresPassword above, for NODES_CALLBACK_TOKEN_SECRET.
*/}}
{{- define "culture-nodes.callbackTokenSecret" -}}
{{- if .Values.callback.tokenSecret -}}
{{- .Values.callback.tokenSecret -}}
{{- else -}}
{{- $secretName := printf "%s-callback" (include "culture-nodes.fullname" .) -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace $secretName -}}
{{- if $existing -}}
{{- index $existing.data "NODES_CALLBACK_TOKEN_SECRET" | b64dec -}}
{{- else -}}
{{- randAlphaNum 32 -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
culture-nodes.callbackBaseURL resolves NODES_CALLBACK_BASE_URL: an explicit
values.callback.baseURL wins, then the Ingress host (when enabled), then
the api Service's in-cluster DNS name — always non-empty, since
cmd/nodes/worker.go's callbackConfig refuses a configured secret with no
base URL.
*/}}
{{- define "culture-nodes.callbackBaseURL" -}}
{{- if .Values.callback.baseURL -}}
{{- .Values.callback.baseURL -}}
{{- else if .Values.ingress.enabled -}}
{{- $scheme := "http" -}}
{{- if .Values.ingress.tls.enabled -}}{{- $scheme = "https" -}}{{- end -}}
{{- printf "%s://%s" $scheme .Values.ingress.host -}}
{{- else -}}
{{- printf "http://%s-api:%v" (include "culture-nodes.fullname" .) .Values.api.service.port -}}
{{- end -}}
{{- end -}}
