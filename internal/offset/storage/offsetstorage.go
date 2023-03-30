package storage

import "github.com/noctarius/event-stream-prototype/internal/offset"

type OffsetStorage interface {
	Start() error
	Stop() error
	Save() error
	Load() error
	Get() (map[string]offset.Offset, error)
	Set(key string, value offset.Offset) error
}