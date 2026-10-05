package core

type MetricKind string

const (
	MetricKindDuration MetricKind = "duration"
	MetricKindInt      MetricKind = "int"
	MetricKindString   MetricKind = "string"
	MetricKindBool     MetricKind = "bool"
)

type MetricSchema struct {
	Key  string
	Kind MetricKind
	Name string
}

type MetricValue struct {
	Key   string
	Value any
}
