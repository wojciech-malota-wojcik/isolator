package wire

import (
	"encoding/json"
	"io"
	"net"
	"os"
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
	// ConfigureSystem tells executor to mount standard mounts like /proc, /dev, /tmp ...  and configure DNS inside new root.
	ConfigureSystem bool

	// IP is the IP to assign executor to.
	IP *net.IPNet

	// Hostname is the hostname to set inside namespace.
	Hostname string

	// DNS is the list of DnS servers to configure inside namespace.
	DNS []net.IP

	// Hosts is the list of hosts and their IP addresses to resolve inside namespace.
	Hosts map[string]net.IP

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

// RunDockerContainer runs docker container.
type RunDockerContainer struct {
	// Path were cached downloads are stored.
	CacheDir string

	// Name is the name of the container.
	Name string

	// Image is the name of the image.
	Image string

	// Tag is the tag of the image.
	Tag string

	// EnvVars sets environment variables inside container.
	EnvVars map[string]string

	// User is the username or UID and group name or GID to use.
	User string

	// WorkingDir is the path to wrking drectory inside the container.
	WorkingDir string

	// Entrypoint for container.
	Entrypoint []string

	// Args is a list of arguments for the container.
	Args []string
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

// ToStream converts stream value to stdin or stderr.
func ToStream(stream Stream) (*os.File, error) {
	var f *os.File
	switch stream {
	case StreamOut:
		f = os.Stdout
	case StreamErr:
		f = os.Stderr
	default:
		return nil, errors.Errorf("unknown stream: %d", stream)
	}
	return f, nil
}
