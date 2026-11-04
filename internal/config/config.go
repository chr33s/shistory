package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

type Config struct {
	Database struct {
		Path string `toml:"path"`
	} `toml:"database"`
	Root struct {
		Git     bool     `toml:"git"`
		VCS     *bool    `toml:"vcs"`
		Markers []string `toml:"markers"`
	} `toml:"root"`
	History struct {
		IgnoreLeadingSpace bool     `toml:"ignore_leading_space"`
		StoreFailed        bool     `toml:"store_failed"`
		MaxResults         int      `toml:"max_results"`
		Ignore             []string `toml:"ignore"`
	} `toml:"history"`
	Search struct {
		Scope  string `toml:"scope"`
		Rank   string `toml:"rank"`
		Dedupe bool   `toml:"dedupe"`
	} `toml:"search"`
	Context struct {
		Env     []string `toml:"env"`
		Window  int      `toml:"window"`
		Weights Weights  `toml:"weights"`
	} `toml:"context"`
	ignore []*regexp.Regexp
}

// Weights multiply frecency in context ranking: each factor is 1 + weight×signal,
// except failure, which is 1 − weight×failure ratio.
type Weights struct {
	Sequence  float64 `toml:"sequence"`
	Directory float64 `toml:"directory"`
	Branch    float64 `toml:"branch"`
	Env       float64 `toml:"env"`
	Session   float64 `toml:"session"`
	Failure   float64 `toml:"failure"`
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ContextEnv encodes the configured environment variables that are set, as a
// JSON object with sorted keys. It is empty when no variables are configured.
func (c Config) ContextEnv(lookup func(string) (string, bool)) string {
	if len(c.Context.Env) == 0 {
		return ""
	}
	values := map[string]string{}
	for _, name := range c.Context.Env {
		if v, ok := lookup(name); ok {
			values[name] = v
		}
	}
	b, _ := json.Marshal(values)
	return string(b)
}

func Expand(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if path == "~" {
			path = home
		} else {
			path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return filepath.Abs(path)
}

func Load() (Config, error) {
	var c Config
	home, err := os.UserHomeDir()
	if err != nil {
		return c, err
	}
	data := os.Getenv("XDG_DATA_HOME")
	if data == "" {
		data = filepath.Join(home, ".local", "share")
	}
	c.Database.Path = filepath.Join(data, "shistory", "history.db")
	c.Root.Git = true
	c.History.IgnoreLeadingSpace, c.History.StoreFailed, c.History.MaxResults = true, true, 100
	c.Search.Scope, c.Search.Dedupe, c.Search.Rank = "auto", true, "frecent"
	c.Context.Window = 2000
	c.Context.Weights = Weights{Sequence: 4, Directory: 1, Branch: 1, Env: 1, Session: 0.5, Failure: 0.5}
	path := os.Getenv("SHISTORY_CONFIG")
	explicit := path != ""
	if !explicit {
		dir := os.Getenv("XDG_CONFIG_HOME")
		if dir == "" {
			dir = filepath.Join(home, ".config")
		}
		path = filepath.Join(dir, "shistory", "config.toml")
	}
	path, err = Expand(path)
	if err != nil {
		return c, err
	}
	b, err := os.ReadFile(path)
	if err != nil && (!os.IsNotExist(err) || explicit) {
		return c, err
	}
	if err == nil {
		if err = toml.NewDecoder(bytes.NewReader(b)).DisallowUnknownFields().Decode(&c); err != nil {
			return c, fmt.Errorf("config: %w", err)
		}
	}
	if c.Root.VCS != nil {
		c.Root.Git = *c.Root.VCS
	}
	if override := os.Getenv("SHISTORY_DB"); override != "" {
		c.Database.Path = override
	}
	if c.Database.Path == "" {
		return c, fmt.Errorf("database.path cannot be empty")
	}
	c.Database.Path, err = Expand(c.Database.Path)
	if err != nil {
		return c, err
	}
	if c.History.MaxResults < 1 || c.History.MaxResults > 100000 {
		return c, fmt.Errorf("history.max_results must be between 1 and 100000")
	}
	if c.Search.Scope != "auto" && c.Search.Scope != "global" && c.Search.Scope != "directory" {
		return c, fmt.Errorf("search.scope must be auto, directory, or global")
	}
	if c.Search.Rank != "recent" && c.Search.Rank != "frecent" && c.Search.Rank != "context" {
		return c, fmt.Errorf("search.rank must be recent, frecent, or context")
	}
	if c.Context.Window < 1 || c.Context.Window > 100000 {
		return c, fmt.Errorf("context.window must be between 1 and 100000")
	}
	w := c.Context.Weights
	for _, v := range []float64{w.Sequence, w.Directory, w.Branch, w.Env, w.Session, w.Failure} {
		if v < 0 || v > 100 {
			return c, fmt.Errorf("context.weights must be between 0 and 100")
		}
	}
	if w.Failure > 1 {
		return c, fmt.Errorf("context.weights.failure must be between 0 and 1")
	}
	for _, name := range c.Context.Env {
		if !envName.MatchString(name) {
			return c, fmt.Errorf("context.env must contain variable names: %q", name)
		}
	}
	for _, marker := range c.Root.Markers {
		if marker == "" || marker == "." || marker == ".." || filepath.Base(marker) != marker {
			return c, fmt.Errorf("root marker must be a filename: %q", marker)
		}
	}
	patterns := append([]string{}, c.History.Ignore...)
	if pattern := os.Getenv("SHISTORY_IGNORE"); pattern != "" {
		patterns = append(patterns, pattern)
	}
	for _, pattern := range patterns {
		r, err := regexp.Compile(pattern)
		if err != nil {
			return c, fmt.Errorf("ignore pattern: %w", err)
		}
		c.ignore = append(c.ignore, r)
	}
	return c, nil
}

func (c Config) Ignore(command string, status *int) bool {
	if strings.TrimSpace(command) == "" || (c.History.IgnoreLeadingSpace && strings.HasPrefix(command, " ")) {
		return true
	}
	if !c.History.StoreFailed && status != nil && *status != 0 {
		return true
	}
	for _, r := range c.ignore {
		if r.MatchString(command) {
			return true
		}
	}
	return false
}
