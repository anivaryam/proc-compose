package runner

import (
	"github.com/shirou/gopsutil/v3/process"
)

func collectMetrics(pid int) (cpu float64, mem float64, err error) {
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return 0, 0, err
	}

	cpuPercent, err := p.CPUPercent()
	if err != nil {
		cpuPercent = 0
	}

	memInfo, err := p.MemoryInfo()
	if err != nil {
		memInfo = nil
	}

	var memMB float64
	if memInfo != nil {
		memMB = float64(memInfo.RSS) / (1024 * 1024)
	}

	return cpuPercent, memMB, nil
}