package settings

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/viper"
)

const (
	ConfigDirName  = ".vitistack"
	ConfigFileName = "ctl.config"
	ConfigFileType = "yaml"

	KeyAvailabilityZones = "availabilityzones"
)

// AvailabilityZone points at a single Kubernetes cluster that is part of a
// Vitistack deployment. Either Kubeconfig, Context, or both may be set. An
// empty Kubeconfig falls back to $KUBECONFIG or ~/.kube/config; an empty
// Context uses the kubeconfig's current-context.
type AvailabilityZone struct {
	Name       string `mapstructure:"name"       yaml:"name"`
	Kubeconfig string `mapstructure:"kubeconfig" yaml:"kubeconfig,omitempty"`
	Context    string `mapstructure:"context"    yaml:"context,omitempty"`
}

// Init loads ~/.vitistack/ctl.config.yaml into Viper. A missing file is not
// an error; commands that need cluster access will surface a clearer message
// when no availability zones are configured.
func Init() error {
	dir, err := ConfigDir()
	if err != nil {
		return err
	}
	viper.SetConfigName(ConfigFileName)
	viper.SetConfigType(ConfigFileType)
	viper.AddConfigPath(dir)

	viper.SetEnvPrefix("VITICTL")
	viper.AutomaticEnv()

	if err := viper.ReadInConfig(); err != nil {
		var nf viper.ConfigFileNotFoundError
		if errors.As(err, &nf) {
			return nil
		}
		if _, ok := err.(*os.PathError); ok {
			return nil
		}
		return fmt.Errorf("reading config: %w", err)
	}
	return nil
}

func ConfigDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home directory: %w", err)
	}
	return filepath.Join(home, ConfigDirName), nil
}

func ConfigFilePath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, ConfigFileName+"."+ConfigFileType), nil
}

// AvailabilityZones returns all configured availability zones.
func AvailabilityZones() ([]AvailabilityZone, error) {
	var out []AvailabilityZone
	if err := viper.UnmarshalKey(KeyAvailabilityZones, &out); err != nil {
		return nil, fmt.Errorf("parsing availability zones from config: %w", err)
	}
	return out, nil
}

// AvailabilityZoneByName looks up a single availability zone by name.
func AvailabilityZoneByName(name string) (AvailabilityZone, error) {
	zones, err := AvailabilityZones()
	if err != nil {
		return AvailabilityZone{}, err
	}
	for _, z := range zones {
		if z.Name == name {
			return z, nil
		}
	}
	return AvailabilityZone{}, fmt.Errorf("no availability zone named %q configured", name)
}

// SaveAvailabilityZones replaces the availability-zones list in the config file.
func SaveAvailabilityZones(zones []AvailabilityZone) error {
	viper.Set(KeyAvailabilityZones, availabilityZonesToMap(zones))
	return save()
}

// AddAvailabilityZone appends or replaces an availability zone by name.
func AddAvailabilityZone(z AvailabilityZone) error {
	if z.Name == "" {
		return errors.New("availability zone name is required")
	}
	if z.Kubeconfig == "" && z.Context == "" {
		return errors.New("availability zone requires at least a kubeconfig or a context")
	}
	zones, err := AvailabilityZones()
	if err != nil {
		return err
	}
	replaced := false
	for i, existing := range zones {
		if existing.Name == z.Name {
			zones[i] = z
			replaced = true
			break
		}
	}
	if !replaced {
		zones = append(zones, z)
	}
	return SaveAvailabilityZones(zones)
}

// RemoveAvailabilityZone removes an availability zone by name. Returns an
// error if it does not exist.
func RemoveAvailabilityZone(name string) error {
	zones, err := AvailabilityZones()
	if err != nil {
		return err
	}
	filtered := zones[:0]
	found := false
	for _, z := range zones {
		if z.Name == name {
			found = true
			continue
		}
		filtered = append(filtered, z)
	}
	if !found {
		return fmt.Errorf("no availability zone named %q", name)
	}
	return SaveAvailabilityZones(filtered)
}

func save() error {
	dir, err := ConfigDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}
	path, err := ConfigFilePath()
	if err != nil {
		return err
	}
	return viper.WriteConfigAs(path)
}

// availabilityZonesToMap converts a slice of AvailabilityZone to the generic
// map shape Viper needs so YAML output remains stable regardless of struct
// tag handling.
func availabilityZonesToMap(zones []AvailabilityZone) []map[string]any {
	out := make([]map[string]any, 0, len(zones))
	for _, z := range zones {
		m := map[string]any{"name": z.Name}
		if z.Kubeconfig != "" {
			m["kubeconfig"] = z.Kubeconfig
		}
		if z.Context != "" {
			m["context"] = z.Context
		}
		out = append(out, m)
	}
	return out
}
