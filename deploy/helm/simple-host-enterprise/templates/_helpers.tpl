{{/* True once the values the server cannot start without are set. */}}
{{- define "sh.configured" -}}
{{- if and .Values.host .Values.oidc.issuer .Values.oidc.clientId .Values.storage.bucket -}}true{{- end -}}
{{- end -}}

{{- define "sh.image" -}}
{{- if not (regexMatch "^sha256:[a-f0-9]{64}$" .Values.image.digest) -}}
{{- fail "image.digest must be a sha256 digest; the chart never pulls by tag" -}}
{{- end -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- end -}}

{{- define "sh.labels" -}}
app.kubernetes.io/name: simple-host-enterprise
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end -}}

{{- define "sh.secretName" -}}
{{- .Values.secrets.existingSecret | default "simple-host-secrets" -}}
{{- end -}}

{{/* The database CA Secret the pods mount, if any. */}}
{{- define "sh.dbCASecret" -}}
{{- if eq .Values.postgres.mode "incluster" -}}postgres-tls{{- else if .Values.postgres.external.caCert -}}simple-host-db-ca{{- end -}}
{{- end -}}

{{- define "sh.podSecurity" -}}
runAsNonRoot: true
runAsUser: 65532
runAsGroup: 65532
fsGroup: 65532
seccompProfile:
  type: RuntimeDefault
{{- end -}}

{{- define "sh.containerSecurity" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
capabilities:
  drop: ["ALL"]
{{- end -}}

{{- define "sh.dbCAVolume" -}}
- name: db-ca
  secret:
    secretName: {{ include "sh.dbCASecret" . | default "simple-host-db-ca" }}
    optional: true
    {{- if eq .Values.postgres.mode "incluster" }}
    items:
      - key: ca.crt
        path: ca.crt
    {{- end }}
{{- end -}}

{{- define "sh.scheduling" -}}
{{- with .Values.nodeSelector }}
nodeSelector: {{- toYaml . | nindent 2 }}
{{- end }}
{{- with .Values.tolerations }}
tolerations: {{- toYaml . | nindent 2 }}
{{- end }}
{{- with .Values.affinity }}
affinity: {{- toYaml . | nindent 2 }}
{{- end }}
{{- with .Values.image.pullSecrets }}
imagePullSecrets:
{{- range . }}
  - name: {{ . }}
{{- end }}
{{- end }}
{{- end -}}
