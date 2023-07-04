package wire

import (
	"encoding/json"
	"io"
	"reflect"

	"github.com/pkg/errors"
)

// Mount defines directories to mount inside container
type Mount struct {
	// Location on Host
	Host string

	// Mountpoint inside container
	Container string

	// Writable makes mount writable inside container
	Writable bool
}

// Config stores configuration of executor
type Config struct {
	// NoStandardMounts tells to not mount standard mounts like /proc, /dev, /tmp ... inside new root.
	NoStandardMounts bool

	// Mounts is the list of bindings to apply inside container
	Mounts []Mount
}

// Execute is sent to execute a shell command
type Execute struct {
	// Command is a command to execute
	Command string
}

// InflateDockerImage initializes filesystem by downloading and inflating docker image.
type InflateDockerImage struct {
	// Path were cached downloads are stored.
	CacheDir string

	// Image is the name of the image.
	Image string

	// Tag is the tag of the image.
	Tag string
}

// Result is sent once command finishes
type Result struct {
	// Error is the error returned by command
	Error string
}

// Stream is the type of stream where log was produced
type Stream int

const (
	// StreamOut represents stdout
	StreamOut Stream = iota

	// StreamErr represents stderr
	StreamErr
)

// Log is the log message printed by executed command
type Log struct {
	// Stream is the type of stream where log was produced
	Stream Stream

	// Text is text printed by command
	Text string
}

type message struct {
	Type    string
	Payload json.RawMessage
}

// EncoderFunc is the encoding function.
type EncoderFunc func(content interface{}) error

// NewEncoder creates new message encoder.
func NewEncoder(w io.Writer) EncoderFunc {
	encoder := json.NewEncoder(w)
	return func(content interface{}) error {
		contentRaw, err := json.Marshal(content)
		if err != nil {
			return errors.WithStack(err)
		}

		return errors.WithStack(encoder.Encode(message{
			Type:    ContentToType(content),
			Payload: contentRaw,
		}))
	}
}

// DecoderFunc is the decoding function.
type DecoderFunc func() (interface{}, error)

// NewDecoder creates new message decoder.
func NewDecoder(r io.Reader, types []interface{}) DecoderFunc {
	decoder := json.NewDecoder(r)
	typeMap := typesToMap(types)
	return func() (interface{}, error) {
		var msg message
		if err := decoder.Decode(&msg); err != nil {
			return nil, errors.WithStack(err)
		}

		contentType, exists := typeMap[msg.Type]
		if !exists {
			return nil, errors.Errorf("no content type defined for %s", msg.Type)
		}

		value := reflect.New(reflect.TypeOf(contentType))

		err := json.Unmarshal(msg.Payload, value.Interface())
		if err != nil {
			return nil, errors.WithStack(err)
		}

		return value.Elem().Interface(), nil
	}
}

// ContentToType returns string representation for type of the content.
func ContentToType(content interface{}) string {
	t := reflect.TypeOf(content)
	return t.PkgPath() + "/" + t.Name()
}

func typesToMap(types []interface{}) map[string]interface{} {
	res := map[string]interface{}{}
	for _, t := range types {
		res[ContentToType(t)] = t
	}
	return res
}
