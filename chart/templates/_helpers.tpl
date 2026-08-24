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
