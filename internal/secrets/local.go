package secrets

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/viper"
)

// Local is the dev backend: a JSON file shaped like secret_sample.json.
type Local struct{ Path string }

func (Local) Kind() string { return "local" }

func (l Local) Load(context.Context) (*viper.Viper, error) {
	b, err := os.ReadFile(l.Path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w (copy secret_sample.json to %s and fill it in)", l.Path, err, l.Path)
	}
	return Parse(b)
}
