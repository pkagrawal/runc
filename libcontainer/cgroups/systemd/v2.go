// +build linux

package systemd

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	systemdDbus "github.com/coreos/go-systemd/v22/dbus"
	securejoin "github.com/cyphar/filepath-securejoin"
	"github.com/opencontainers/runc/libcontainer/cgroups"
	"github.com/opencontainers/runc/libcontainer/cgroups/fs2"
	"github.com/opencontainers/runc/libcontainer/configs"
	"github.com/pkg/errors"
)

type unifiedManager struct {
	mu      sync.Mutex
	cgroups *configs.Cgroup
	// path is like "/sys/fs/cgroup/user.slice/user-1001.slice/session-1.scope"
	path     string
	rootless bool
}

func NewUnifiedManager(config *configs.Cgroup, path string, rootless bool) cgroups.Manager {
	return &unifiedManager{
		cgroups:  config,
		path:     path,
		rootless: rootless,
	}
}

func genV2ResourcesProperties(c *configs.Cgroup) ([]systemdDbus.Property, error) {
	var properties []systemdDbus.Property

	if c.Resources.Memory != 0 {
		properties = append(properties,
			newProp("MemoryMax", uint64(c.Resources.Memory)))
	}
	// swap is set
	if c.Resources.MemorySwap != 0 {
		swap, err := cgroups.ConvertMemorySwapToCgroupV2Value(c.Resources.MemorySwap, c.Resources.Memory)
		if err != nil {
			return nil, err
		}
		properties = append(properties,
			newProp("MemorySwapMax", uint64(swap)))
	}

	if c.Resources.CpuWeight != 0 {
		properties = append(properties,
			newProp("CPUWeight", c.Resources.CpuWeight))
	}

	// cpu.cfs_quota_us and cpu.cfs_period_us are controlled by systemd.
	if c.Resources.CpuQuota != 0 && c.Resources.CpuPeriod != 0 {
		// corresponds to USEC_INFINITY in systemd
		// if USEC_INFINITY is provided, CPUQuota is left unbound by systemd
		// always setting a property value ensures we can apply a quota and remove it later
		cpuQuotaPerSecUSec := uint64(math.MaxUint64)
		if c.Resources.CpuQuota > 0 {
			// systemd converts CPUQuotaPerSecUSec (microseconds per CPU second) to CPUQuota
			// (integer percentage of CPU) internally.  This means that if a fractional percent of
			// CPU is indicated by Resources.CpuQuota, we need to round up to the nearest
			// 10ms (1% of a second) such that child cgroups can set the cpu.cfs_quota_us they expect.
			cpuQuotaPerSecUSec = uint64(c.Resources.CpuQuota*1000000) / c.Resources.CpuPeriod
			if cpuQuotaPerSecUSec%10000 != 0 {
				cpuQuotaPerSecUSec = ((cpuQuotaPerSecUSec / 10000) + 1) * 10000
			}
		}
		properties = append(properties,
			newProp("CPUQuotaPerSecUSec", cpuQuotaPerSecUSec))
	}

	if c.Resources.PidsLimit > 0 {
		properties = append(properties,
			newProp("TasksAccounting", true),
			newProp("TasksMax", uint64(c.Resources.PidsLimit)))
	}

	// ignore c.Resources.KernelMemory

	return properties, nil
}

func (m *unifiedManager) Apply(pid int) error {
	var (
		c          = m.cgroups
		unitName   = getUnitName(c)
		properties []systemdDbus.Property
	)

	if c.Paths != nil {
		return cgroups.WriteCgroupProc(m.path, pid)
	}

	slice := "system.slice"
	if m.rootless {
		slice = "user.slice"
	}
	if c.Parent != "" {
		slice = c.Parent
	}

	properties = append(properties, systemdDbus.PropDescription("libcontainer container "+c.Name))

	// if we create a slice, the parent is defined via a Wants=
	if strings.HasSuffix(unitName, ".slice") {
		properties = append(properties, systemdDbus.PropWants(slice))
	} else {
		// otherwise, we use Slice=
		properties = append(properties, systemdDbus.PropSlice(slice))
	}

	// only add pid if its valid, -1 is used w/ general slice creation.
	if pid != -1 {
		properties = append(properties, newProp("PIDs", []uint32{uint32(pid)}))
	}

	// Check if we can delegate. This is only supported on systemd versions 218 and above.
	if !strings.HasSuffix(unitName, ".slice") {
		// Assume scopes always support delegation.
		properties = append(properties, newProp("Delegate", true))
	}

	// Always enable accounting, this gets us the same behaviour as the fs implementation,
	// plus the kernel has some problems with joining the memory cgroup at a later time.
	properties = append(properties,
		newProp("MemoryAccounting", true),
		newProp("CPUAccounting", true),
		newProp("IOAccounting", true))

	// Assume DefaultDependencies= will always work (the check for it was previously broken.)
	properties = append(properties,
		newProp("DefaultDependencies", false))

	resourcesProperties, err := genV2ResourcesProperties(c)
	if err != nil {
		return err
	}
	properties = append(properties, resourcesProperties...)
	properties = append(properties, c.SystemdProps...)

	dbusConnection, err := getDbusConnection(m.rootless)
	if err != nil {
		return err
	}
	if err := startUnit(dbusConnection, unitName, properties); err != nil {
		return errors.Wrapf(err, "error while starting unit %q with properties %+v", unitName, properties)
	}

	_, err = m.GetUnifiedPath()
	if err != nil {
		return err
	}
	if err := fs2.CreateCgroupPath(m.path, m.cgroups); err != nil {
		return err
	}
	return nil
}

func (m *unifiedManager) Destroy() error {
	if m.cgroups.Paths != nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	dbusConnection, err := getDbusConnection(m.rootless)
	if err != nil {
		return err
	}
	unitName := getUnitName(m.cgroups)
	if err := stopUnit(dbusConnection, unitName); err != nil {
		return err
	}

	// XXX this is probably not needed, systemd should handle it
	err = os.Remove(m.path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	return nil
}

// this method is for v1 backward compatibility and will be removed
func (m *unifiedManager) GetPaths() map[string]string {
	_, _ = m.GetUnifiedPath()
	paths := map[string]string{
		"pids":    m.path,
		"memory":  m.path,
		"io":      m.path,
		"cpu":     m.path,
		"devices": m.path,
		"cpuset":  m.path,
		"freezer": m.path,
	}
	return paths
}

// getSliceFull value is used in GetUnifiedPath.
// The value is incompatible with systemdDbus.PropSlice.
func (m *unifiedManager) getSliceFull() (string, error) {
	c := m.cgroups
	slice := "system.slice"
	if m.rootless {
		slice = "user.slice"
	}
	if c.Parent != "" {
		var err error
		slice, err = ExpandSlice(c.Parent)
		if err != nil {
			return "", err
		}
	}

	if m.rootless {
		dbusConnection, err := getDbusConnection(m.rootless)
		if err != nil {
			return "", err
		}
		// managerCGQuoted is typically "/user.slice/user-${uid}.slice/user@${uid}.service" including the quote symbols
		managerCGQuoted, err := dbusConnection.GetManagerProperty("ControlGroup")
		if err != nil {
			return "", err
		}
		managerCG, err := strconv.Unquote(managerCGQuoted)
		if err != nil {
			return "", err
		}
		slice = filepath.Join(managerCG, slice)
	}

	// an example of the final slice in rootless: "/user.slice/user-1001.slice/user@1001.service/user.slice"
	// NOTE: systemdDbus.PropSlice requires the "/user.slice/user-1001.slice/user@1001.service/" prefix NOT to be specified.
	return slice, nil
}

func (m *unifiedManager) GetUnifiedPath() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.path != "" {
		return m.path, nil
	}

	sliceFull, err := m.getSliceFull()
	if err != nil {
		return "", err
	}

	c := m.cgroups
	path := filepath.Join(sliceFull, getUnitName(c))
	path, err = securejoin.SecureJoin(fs2.UnifiedMountpoint, path)
	if err != nil {
		return "", err
	}
	m.path = path

	// an example of the final path in rootless:
	// "/sys/fs/cgroup/user.slice/user-1001.slice/user@1001.service/user.slice/libpod-132ff0d72245e6f13a3bbc6cdc5376886897b60ac59eaa8dea1df7ab959cbf1c.scope"
	return m.path, nil
}

func (m *unifiedManager) fsManager() (cgroups.Manager, error) {
	path, err := m.GetUnifiedPath()
	if err != nil {
		return nil, err
	}
	return fs2.NewManager(m.cgroups, path, m.rootless)
}

func (m *unifiedManager) Freeze(state configs.FreezerState) error {
	fsMgr, err := m.fsManager()
	if err != nil {
		return err
	}
	return fsMgr.Freeze(state)
}

func (m *unifiedManager) GetPids() ([]int, error) {
	path, err := m.GetUnifiedPath()
	if err != nil {
		return nil, err
	}
	return cgroups.GetPids(path)
}

func (m *unifiedManager) GetAllPids() ([]int, error) {
	path, err := m.GetUnifiedPath()
	if err != nil {
		return nil, err
	}
	return cgroups.GetAllPids(path)
}

func (m *unifiedManager) GetStats() (*cgroups.Stats, error) {
	fsMgr, err := m.fsManager()
	if err != nil {
		return nil, err
	}
	return fsMgr.GetStats()
}

func (m *unifiedManager) Set(container *configs.Config) error {
	properties, err := genV2ResourcesProperties(m.cgroups)
	if err != nil {
		return err
	}
	dbusConnection, err := getDbusConnection(m.rootless)
	if err != nil {
		return err
	}
	if err := dbusConnection.SetUnitProperties(getUnitName(m.cgroups), true, properties...); err != nil {
		return errors.Wrap(err, "error while setting unit properties")
	}

	fsMgr, err := m.fsManager()
	if err != nil {
		return err
	}
	return fsMgr.Set(container)
}

func (m *unifiedManager) GetCgroups() (*configs.Cgroup, error) {
	return m.cgroups, nil
}
