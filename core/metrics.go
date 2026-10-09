package core

import "time"

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

var BuiltinMetrics = []MetricSchema{
	{Key: "time_to_close", Kind: MetricKindDuration, Name: "Time to close"},
	{Key: "time_to_first_reply", Kind: MetricKindDuration, Name: "Time to first reply"},
	{Key: "total_wait_time", Kind: MetricKindDuration, Name: "Total wait time"},
	{Key: "message_count", Kind: MetricKindInt, Name: "Message count"},
}

func KindOf(v any) MetricKind {
	switch v.(type) {
	case time.Duration:
		return MetricKindDuration
	case int, int32, int64:
		return MetricKindInt
	case bool:
		return MetricKindBool
	default:
		return MetricKindString
	}
}
