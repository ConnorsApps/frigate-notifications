package main

import (
	corev1 "k8s.io/api/core/v1"
)

// These types mirror chart/values.yaml by hand. The schema rejects unknown
// keys, so a missing one fails `helm lint`.

type pullPolicy string

func (pullPolicy) Enum() []any {
	return []any{string(corev1.PullAlways), string(corev1.PullIfNotPresent), string(corev1.PullNever)}
}

type serviceType string

func (serviceType) Enum() []any {
	return []any{
		string(corev1.ServiceTypeClusterIP), string(corev1.ServiceTypeNodePort),
		string(corev1.ServiceTypeLoadBalancer), string(corev1.ServiceTypeExternalName),
	}
}

// traceProtocol: the app uses gRPC when the value starts with "grpc", else HTTP.
type traceProtocol string

func (traceProtocol) Enum() []any { return []any{"http/protobuf", "grpc"} }

// traceSampler is the set the OTel Go SDK reads from OTEL_TRACES_SAMPLER.
type traceSampler string

func (traceSampler) Enum() []any {
	return []any{
		"always_on", "always_off", "traceidratio",
		"parentbased_always_on", "parentbased_always_off", "parentbased_traceidratio",
	}
}

type mongoMode string

func (mongoMode) Enum() []any { return []any{"statefulset", "operator"} }

type imageValues struct {
	Repository string     `json:"repository" description:"Image repository"`
	PullPolicy pullPolicy `json:"pullPolicy" description:"Image pull policy"`
	Tag        string     `json:"tag" description:"Image tag. Defaults to the chart's appVersion."`
}

type serviceValues struct {
	Type      serviceType `json:"type" description:"Kubernetes Service type"`
	Port      int         `json:"port" minimum:"1" maximum:"65535" description:"Serves /healthz, /readyz and metrics — cluster-internal only"`
	MediaPort int         `json:"mediaPort" minimum:"1" maximum:"65535" description:"Serves signed media links and is the only port routed publicly"`
}

type otelTraces struct {
	Endpoint   string        `json:"endpoint" description:"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT. Used as-is, so over HTTP include the full path (e.g. .../v1/traces); for gRPC give host:port or a URL."`
	Protocol   traceProtocol `json:"protocol" description:"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL: http/protobuf or grpc"`
	Sampler    traceSampler  `json:"sampler" description:"OTEL_TRACES_SAMPLER. With parentbased_traceidratio, set samplerArg to e.g. \"0.1\" to sample 10%."`
	SamplerArg string        `json:"samplerArg" description:"OTEL_TRACES_SAMPLER_ARG, e.g. \"0.1\" (quote it: it must be a string)"`
}

type otelMetrics struct {
	Endpoint       string `json:"endpoint" description:"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT, used as-is (include the full path)"`
	ExportInterval int    `json:"exportInterval" minimum:"1" description:"OTEL_METRIC_EXPORT_INTERVAL, milliseconds"`
}

type otelValues struct {
	Endpoint             string            `json:"endpoint" description:"OTEL_EXPORTER_OTLP_ENDPOINT, the base URL for both signals. Over HTTP the SDK appends /v1/traces and /v1/metrics, so use the per-signal endpoints when the backend serves a different path. Export is off until an endpoint is set."`
	Headers              map[string]string `json:"headers" description:"OTEL_EXPORTER_OTLP_HEADERS as a map. Not for secrets; put those in extraEnv with valueFrom."`
	KubernetesAttributes bool              `json:"kubernetesAttributes" description:"Attach k8s.namespace.name, k8s.pod.name and k8s.node.name resource attributes from the downward API"`
	ResourceAttributes   map[string]string `json:"resourceAttributes" description:"Extra OTEL_RESOURCE_ATTRIBUTES, e.g. deployment.environment: prod. Values must not contain \",\" or \"=\"."`
	Traces               otelTraces        `json:"traces"`
	Metrics              otelMetrics       `json:"metrics"`
}

type storage struct {
	Size         quantity `json:"size" description:"Volume size, e.g. 5Gi"`
	StorageClass string   `json:"storageClass" description:"StorageClass name. Empty: the cluster default."`
}

type valkeyPersistence struct {
	Enabled      bool     `json:"enabled" description:"Off: emptyDir, lost on reschedule (the app fails open)"`
	Size         quantity `json:"size" description:"Volume size, e.g. 1Gi"`
	StorageClass string   `json:"storageClass" description:"StorageClass name. Empty: the cluster default."`
}

type valkeyValues struct {
	Enabled     bool                        `json:"enabled" description:"Deploy Valkey for cooldowns, review state and rate caps. No auth: keep cluster-internal. Sets REDIS_URL, so leave config.redis.url unset."`
	Image       string                      `json:"image" description:"Valkey image"`
	Persistence valkeyPersistence           `json:"persistence"`
	Resources   corev1.ResourceRequirements `json:"resources"`
	ExtraArgs   []string                    `json:"extraArgs" description:"Extra valkey-server arguments, appended to the container args"`
	ExtraEnv    []corev1.EnvVar             `json:"extraEnv" description:"Extra container env vars"`
	ExtraSpec   map[string]any              `json:"extraSpec" description:"Deep-merged over the StatefulSet spec (maps merge, lists replace), e.g. template.spec.nodeSelector"`
}

type postgresValues struct {
	Enabled   bool                        `json:"enabled" description:"Deploy a CloudNativePG Cluster (needs the CloudNativePG operator installed). Sets DB_URL, so leave config.db.url unset. Mutually exclusive with mongo."`
	ImageName string                      `json:"imageName" description:"Empty: the operator's default image"`
	Instances int                         `json:"instances" minimum:"1" description:"Number of Postgres instances"`
	Database  string                      `json:"database" description:"Database created at bootstrap"`
	Owner     string                      `json:"owner" description:"Database owner created at bootstrap"`
	Storage   storage                     `json:"storage"`
	Resources corev1.ResourceRequirements `json:"resources"`
	ExtraSpec map[string]any              `json:"extraSpec" description:"Deep-merged over the Cluster spec (maps merge, lists replace), e.g. postgresql.parameters"`
}

type mongoValues struct {
	Enabled   bool                        `json:"enabled" description:"Deploy MongoDB. Sets DB_URL, so leave config.db.url unset. Mutually exclusive with postgres."`
	Mode      mongoMode                   `json:"mode" description:"statefulset: standalone official mongo image, no operator. operator: a MongoDBCommunity resource; needs the MongoDB operator installed."`
	Image     string                      `json:"image" description:"Mongo image. Must be >= 8.0.30 (or 8.3.9): earlier builds don't start on Linux >= 6.19 (SERVER-121912)."`
	User      string                      `json:"user" description:"Database user. The password is generated into <release>-mongo-auth."`
	Members   int                         `json:"members" minimum:"1" description:"Replica set members. Operator mode only."`
	Storage   storage                     `json:"storage"`
	Resources corev1.ResourceRequirements `json:"resources"`
	ExtraEnv  []corev1.EnvVar             `json:"extraEnv" description:"Extra container env vars. StatefulSet mode only."`
	ExtraSpec map[string]any              `json:"extraSpec" description:"Deep-merged over the resource's spec (maps merge, lists replace): additionalMongodConfig in operator mode, the StatefulSet spec in statefulset mode"`
}

type values struct {
	ReplicaCount       int                           `json:"replicaCount" minimum:"0" description:"Number of app replicas"`
	Image              imageValues                   `json:"image"`
	ImagePullSecrets   []corev1.LocalObjectReference `json:"imagePullSecrets" description:"Secrets used to pull the app image"`
	NameOverride       string                        `json:"nameOverride" pattern:"^([a-z0-9]([-a-z0-9]*[a-z0-9])?)?$" description:"Override the chart name in resource names"`
	FullnameOverride   string                        `json:"fullnameOverride" pattern:"^([a-z0-9]([-a-z0-9]*[a-z0-9])?)?$" description:"Override the fully qualified resource name"`
	PodAnnotations     map[string]string             `json:"podAnnotations" description:"Extra pod annotations"`
	PodSecurityContext corev1.PodSecurityContext     `json:"podSecurityContext" description:"Pod-level security context"`
	SecurityContext    corev1.SecurityContext        `json:"securityContext" description:"App container security context"`
	Service            serviceValues                 `json:"service"`
	Resources          corev1.ResourceRequirements   `json:"resources" description:"App container resources"`
	Otel               otelValues                    `json:"otel" description:"OpenTelemetry export, rendered as the standard OTEL_* environment variables. Off until an endpoint is set. Metrics are OTLP/HTTP only; traces can use HTTP or gRPC."`
	ExtraEnv           []corev1.EnvVar               `json:"extraEnv" description:"Extra container env vars, appended last so they override anything the chart sets"`
	Valkey             valkeyValues                  `json:"valkey" description:"Optional bundled state store"`
	Postgres           postgresValues                `json:"postgres" description:"Optional bundled PostgreSQL (CloudNativePG)"`
	Mongo              mongoValues                   `json:"mongo" description:"Optional bundled MongoDB"`
	NodeSelector       map[string]string             `json:"nodeSelector" description:"Pod node selector"`
	Tolerations        []corev1.Toleration           `json:"tolerations" description:"Pod tolerations"`
	Affinity           corev1.Affinity               `json:"affinity" description:"Pod affinity rules"`
	Config             config                        `json:"config" description:"Full config.yaml content (see config-example.yaml), rendered into a Secret mounted at /etc/frigate-notify/config.yaml"`
}
