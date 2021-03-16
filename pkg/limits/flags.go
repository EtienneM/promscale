// This file and its contents are licensed under the Apache License 2.0.
// Please see the included NOTICE for copyright information and
// LICENSE for a copy of the license

package limits

import (
	"flag"
	"fmt"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/timescale/promscale/pkg/limits/mem"
	"github.com/timescale/promscale/pkg/util"
)

var (
	MemoryTargetMetric = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: util.PromNamespace,
			Name:      "memory_target_bytes",
			Help:      "The number of bytes of memory the system will target using",
		})
)

type PercentageBytes struct {
	percentage int    //integer [1-100], only set if specified as percentage
	bytes      uint64 //always set
}

func (t *PercentageBytes) SetPercent(percent int) {
	t.percentage = percent
	t.bytes = 0
}

func (t *PercentageBytes) SetBytes(bytes uint64) {
	t.percentage = 0
	t.bytes = bytes
}

func (t *PercentageBytes) Get() (int, uint64) {
	return t.percentage, t.bytes
}

func (t *PercentageBytes) Set(val string) error {
	val = strings.TrimSpace(val)
	percentage := false
	if val[len(val)-1] == '%' {
		percentage = true
		val = val[:len(val)-1]
	}

	numeric, err := strconv.ParseInt(val, 10, 64)
	if err != nil {
		return fmt.Errorf("Cannot parse target memory: %w", err)
	}

	if percentage {
		if numeric < 1 || numeric > 100 {
			return fmt.Errorf("Cannot set target memory: percentage must be in the [1,100] range")
		}
		t.SetPercent(int(numeric))
		return nil
	}
	if numeric < 1000 {
		return fmt.Errorf("Cannot set target memory: must be more than a 1000 bytes")
	}
	t.SetBytes(uint64(numeric))
	return nil
}

func (t *PercentageBytes) String() string {
	if t.percentage > 0 {
		return fmt.Sprintf("%d%%", t.percentage)
	}
	return fmt.Sprintf("%d", t.bytes)
}

func (t *PercentageBytes) Bytes() uint64 {
	return t.bytes
}

type Config struct {
	targetMemoryFlag  PercentageBytes
	TargetMemoryBytes uint64
}

// ParseFlags parses the configuration flags for logging.
func ParseFlags(fs *flag.FlagSet, cfg *Config) *Config {
	/* set defaults */
	sysMem := mem.SystemMemory()
	if sysMem > 0 {
		cfg.targetMemoryFlag.SetPercent(80)
	} else {
		cfg.targetMemoryFlag.SetBytes(1e9) //1 GB if cannot determine system memory.
	}

	fs.Var(&cfg.targetMemoryFlag, "memory-target", "Target for amount of memory to use. "+
		"Specified in bytes or as a percentage of system memory (e.g. 80%).")
	return cfg
}

func Validate(cfg *Config) error {
	if cfg.targetMemoryFlag.percentage > 0 {
		sysMemory := mem.SystemMemory()
		if sysMemory == 0 {
			return fmt.Errorf("Cannot set target memory: specified in percentage terms but total system memory could not be determined")
		}
		cfg.TargetMemoryBytes = uint64(float64(sysMemory) * (float64(cfg.targetMemoryFlag.percentage) / 100.0))
	} else {
		cfg.TargetMemoryBytes = cfg.targetMemoryFlag.bytes
	}
	MemoryTargetMetric.Set(float64(cfg.TargetMemoryBytes))

	return nil
}

func init() {
	prometheus.MustRegister(
		MemoryTargetMetric,
	)
}
