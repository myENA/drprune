package models

type Config struct {
	ReleaseTags []string `decoder:"release_tags,json"`

	// Minimum number of releases to be kept.
	MinReleaseImages int `decoder:"min_release_images"`

	// Minimum days before eviction is considered for release images.
	MinReleaseEvictionDays int `decoder:"min_release_eviction_days"`

	// Minimum days before eviction is considered for all other images.
	MinFeatureEvictionDays int `decoder:"min_feature_eviction_days"`
}

func DefaultConfig() *Config {
	return &Config{
		ReleaseTags:            []string{"master", "release", "latest"},
		MinReleaseImages:       5,
		MinReleaseEvictionDays: 30,
		MinFeatureEvictionDays: 7,
	}
}

func (c *Config) Clone() *Config {
	cfg := &Config{}
	*cfg = *c
	cfg.ReleaseTags = make([]string, len(c.ReleaseTags))
	copy(cfg.ReleaseTags, c.ReleaseTags)
	return cfg
}
