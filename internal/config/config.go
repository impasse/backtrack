package config

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", n.Value, err)
	}
	*d = Duration(v)
	return nil
}

type ByteSize int64

func (b *ByteSize) UnmarshalYAML(n *yaml.Node) error {
	v := strings.TrimSpace(strings.ToUpper(n.Value))
	mult := int64(1)
	for suffix, value := range map[string]int64{"KIB": 1 << 10, "MIB": 1 << 20, "GIB": 1 << 30, "KB": 1000, "MB": 1000 * 1000, "GB": 1000 * 1000 * 1000} {
		if strings.HasSuffix(v, suffix) {
			mult = value
			v = strings.TrimSpace(strings.TrimSuffix(v, suffix))
			break
		}
	}
	num, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid byte size %q", n.Value)
	}
	if num < 0 || num > math.MaxInt64/mult {
		return fmt.Errorf("byte size %q is out of range", n.Value)
	}
	*b = ByteSize(num * mult)
	return nil
}

type Config struct {
	Server struct {
		HTTPListen    string `yaml:"http_listen"`
		RTMPListen    string `yaml:"rtmp_listen"`
		PublicURL     string `yaml:"public_url"`
		AdminToken    string `yaml:"admin_token"`
		ReadOnlyToken string `yaml:"read_only_token"`
		StaticDir     string `yaml:"static_dir"`
	} `yaml:"server"`
	Ingest struct {
		ReconnectGrace         Duration `yaml:"reconnect_grace"`
		ReadTimeout            Duration `yaml:"read_timeout"`
		MaxBitrateMbps         int      `yaml:"max_bitrate_mbps"`
		MaxPublishersPerStream int      `yaml:"max_publishers_per_stream"`
	} `yaml:"ingest"`
	Media struct {
		TargetSegmentDuration Duration `yaml:"target_segment_duration"`
		MaxSegmentDuration    Duration `yaml:"max_segment_duration"`
		KeyframeWaitTimeout   Duration `yaml:"keyframe_wait_timeout"`
		MaxPacketQueue        int      `yaml:"max_packet_queue"`
	} `yaml:"media"`
	Playback struct {
		LivePlaylistDuration Duration `yaml:"live_playlist_duration"`
		DefaultDVRWindow     Duration `yaml:"default_dvr_window"`
		LiveLatencyTarget    Duration `yaml:"live_latency_target"`
	} `yaml:"playback"`
	Storage struct {
		Root              string   `yaml:"root"`
		Database          string   `yaml:"database"`
		ReservedFreeSpace ByteSize `yaml:"reserved_free_space"`
	} `yaml:"storage"`
	Retention struct {
		MaxAge          Duration `yaml:"max_age"`
		MaxSize         ByteSize `yaml:"max_size"`
		CleanupInterval Duration `yaml:"cleanup_interval"`
		PreservePinned  bool     `yaml:"preserve_pinned"`
	} `yaml:"retention"`
	Clips struct {
		MaxDuration            Duration `yaml:"max_duration"`
		MaxConcurrentJobs      int      `yaml:"max_concurrent_jobs"`
		MaxQueueSize           int      `yaml:"max_queue_size"`
		MaxOutputSize          ByteSize `yaml:"max_output_size"`
		LibraryMaxSize         ByteSize `yaml:"library_max_size"`
		TemporaryMaxSize       ByteSize `yaml:"temporary_max_size"`
		JobTimeout             Duration `yaml:"job_timeout"`
		TemporaryFileRetention Duration `yaml:"temporary_file_retention"`
		DefaultMode            string   `yaml:"default_mode"`
		DefaultGapPolicy       string   `yaml:"default_gap_policy"`
	} `yaml:"clips"`
	Recovery struct {
		ScanOnStartup             bool `yaml:"scan_on_startup"`
		QuarantineInvalidSegments bool `yaml:"quarantine_invalid_segments"`
		IntegrityCheck            bool `yaml:"integrity_check"`
	} `yaml:"recovery"`
	Logging struct {
		Level  string `yaml:"level"`
		Format string `yaml:"format"`
	} `yaml:"logging"`
}

func Load(path string) (Config, error) {
	var cfg Config
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	if err = yaml.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}
	if err = cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if c.Server.HTTPListen == "" || c.Server.RTMPListen == "" {
		return errors.New("server.http_listen and server.rtmp_listen are required")
	}
	if c.Server.PublicURL != "" {
		parsed, err := url.ParseRequestURI(c.Server.PublicURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
			return errors.New("server.public_url must use http or https")
		}
	}
	if len(c.Server.AdminToken) < 24 || c.Server.AdminToken == "change-me-before-exposing-this-server" {
		return errors.New("server.admin_token must be a unique value of at least 24 characters")
	}
	if c.Server.ReadOnlyToken != "" && (len(c.Server.ReadOnlyToken) < 24 || c.Server.ReadOnlyToken == c.Server.AdminToken) {
		return errors.New("server.read_only_token must be empty or a unique value of at least 24 characters")
	}
	if c.Storage.Root == "" || c.Storage.Database == "" {
		return errors.New("storage.root and storage.database are required")
	}
	if c.Media.TargetSegmentDuration <= 0 || c.Media.MaxSegmentDuration < c.Media.TargetSegmentDuration {
		return errors.New("media segment durations are invalid")
	}
	if c.Ingest.ReconnectGrace < 0 || c.Ingest.ReadTimeout <= 0 || c.Ingest.MaxBitrateMbps <= 0 {
		return errors.New("ingest reconnect_grace must be non-negative and read_timeout/max_bitrate_mbps must be positive")
	}
	if c.Ingest.MaxPublishersPerStream != 1 {
		return errors.New("ingest.max_publishers_per_stream must be 1 in the initial single-publisher implementation")
	}
	if c.Media.KeyframeWaitTimeout <= 0 || c.Media.MaxPacketQueue <= 0 {
		return errors.New("media.keyframe_wait_timeout and max_packet_queue must be positive")
	}
	if c.Playback.LivePlaylistDuration <= 0 || c.Playback.DefaultDVRWindow <= 0 || c.Playback.LiveLatencyTarget < 0 {
		return errors.New("playback windows must be positive and live_latency_target must be non-negative")
	}
	if c.Storage.ReservedFreeSpace < 0 {
		return errors.New("storage.reserved_free_space must be non-negative")
	}
	if c.Retention.MaxAge <= 0 || c.Retention.MaxSize <= 0 || c.Retention.CleanupInterval <= 0 {
		return errors.New("retention max_age, max_size, and cleanup_interval must be positive")
	}
	if c.Clips.MaxDuration <= 0 || c.Clips.MaxConcurrentJobs <= 0 || c.Clips.MaxQueueSize <= 0 || c.Clips.MaxOutputSize <= 0 || c.Clips.JobTimeout <= 0 || c.Clips.TemporaryFileRetention < 0 {
		return errors.New("clip duration, worker/queue limits, output size, and timeout must be positive; temporary retention must be non-negative")
	}
	if c.Clips.LibraryMaxSize < 0 || c.Clips.TemporaryMaxSize < 0 {
		return errors.New("clip library and temporary storage limits must be non-negative")
	}
	if c.Clips.DefaultMode != "accurate" && c.Clips.DefaultMode != "fast" {
		return errors.New("clips.default_mode must be fast or accurate")
	}
	if c.Clips.DefaultGapPolicy != "reject" && c.Clips.DefaultGapPolicy != "skip" && c.Clips.DefaultGapPolicy != "fill" {
		return errors.New("clips.default_gap_policy is invalid")
	}
	if c.Logging.Level != "" && c.Logging.Level != "debug" && c.Logging.Level != "info" && c.Logging.Level != "warn" && c.Logging.Level != "error" {
		return errors.New("logging.level must be debug, info, warn, or error")
	}
	if c.Logging.Format != "" && c.Logging.Format != "json" && c.Logging.Format != "text" {
		return errors.New("logging.format must be json or text")
	}
	return nil
}
