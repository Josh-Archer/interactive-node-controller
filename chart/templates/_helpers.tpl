{{- define "interactive-node-controller.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "interactive-node-controller.fullname" -}}
{{- if .Values.fullnameOverride }}{{ .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}{{ else }}{{ printf "%s-%s" .Release.Name (include "interactive-node-controller.name" .) | trunc 63 | trimSuffix "-" }}{{ end }}
{{- end }}

{{- define "interactive-node-controller.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}{{ default (include "interactive-node-controller.fullname" .) .Values.serviceAccount.name }}{{ else }}{{ required "serviceAccount.name is required when create is false" .Values.serviceAccount.name }}{{ end }}
{{- end }}

{{- define "interactive-node-controller.image" -}}
{{- if .Values.image.digest }}{{ printf "%s@%s" .Values.image.repository .Values.image.digest }}{{ else }}{{ printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) }}{{ end }}
{{- end }}

{{/*
burstTaintKey returns taint.burstKey when it is safe to pass to --burst-taint-key.
An empty value returns nothing so the Deployment keeps its single-taint arguments.
Rules match controller.TaintPolicy.Validate and Kubernetes validation.IsQualifiedName:
optional DNS subdomain prefix, one slash, and a 1-63 character name.
*/}}
{{- define "interactive-node-controller.burstTaintKey" -}}
{{- $raw := .Values.taint.burstKey -}}
{{- if not (kindIs "string" $raw) -}}
{{- if not (kindIs "invalid" $raw) -}}
{{- fail "taint.burstKey must be a string" -}}
{{- end -}}
{{- else if ne $raw "" -}}
{{- if or (contains " " $raw) (contains "\t" $raw) (contains "\n" $raw) (contains "\r" $raw) -}}
{{- fail (printf "taint.burstKey %q cannot contain whitespace" $raw) -}}
{{- end -}}
{{- if eq $raw (.Values.taint.key | toString) -}}
{{- fail (printf "taint.burstKey %q collides with taint.key" $raw) -}}
{{- end -}}
{{- $parts := splitList "/" $raw -}}
{{- if gt (len $parts) 2 -}}
{{- fail (printf "taint.burstKey %q is not a valid Kubernetes taint key" $raw) -}}
{{- end -}}
{{- $name := $raw -}}
{{- if eq (len $parts) 2 -}}
{{- $prefix := index $parts 0 -}}
{{- $name = index $parts 1 -}}
{{- if or (eq $prefix "") (gt (len $prefix) 253) (not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$" $prefix)) -}}
{{- fail (printf "taint.burstKey %q is not a valid Kubernetes taint key" $raw) -}}
{{- end -}}
{{- end -}}
{{- if or (eq $name "") (gt (len $name) 63) (not (regexMatch "^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$" $name)) -}}
{{- fail (printf "taint.burstKey %q is not a valid Kubernetes taint key" $raw) -}}
{{- end -}}
{{- $raw -}}
{{- end -}}
{{- end -}}

{{- define "interactive-node-controller.burstTaintArg" -}}
{{- $key := include "interactive-node-controller.burstTaintKey" . -}}
{{- if $key }}
            - --burst-taint-key={{ $key }}
{{- end -}}
{{- end -}}
