//go:build linux

package hw

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHardware_LinuxPackageSocketCount(t *testing.T) {
	newCPU := func(root, id, packageID string) {
		topology := filepath.Join(root, id, "topology")
		if err := os.MkdirAll(topology, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(topology, "physical_package_id"), []byte(packageID+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("two packages", func(t *testing.T) {
		dir := t.TempDir()
		newCPU(dir, "cpu0", "0")
		newCPU(dir, "cpu1", "0")
		newCPU(dir, "cpu2", "1")
		if got := packageSocketCount(dir); got != 2 {
			t.Fatalf("packageSocketCount() = %d, want 2", got)
		}
	})
	t.Run("single package", func(t *testing.T) {
		dir := t.TempDir()
		newCPU(dir, "cpu0", "36")
		newCPU(dir, "cpu1", "36")
		if got := packageSocketCount(dir); got != 1 {
			t.Fatalf("packageSocketCount() = %d, want 1", got)
		}
	})
	t.Run("no topology", func(t *testing.T) {
		if got := packageSocketCount(t.TempDir()); got != 0 {
			t.Fatalf("packageSocketCount() = %d, want 0", got)
		}
	})
}

func TestHardware_LinuxSystem(t *testing.T) {
	root := t.TempDir()
	for name, value := range map[string]string{
		"sys_vendor":      "LENOVO\n",
		"product_version": "ThinkStation PGX\n",
		"product_name":    "30KL0004FC\n",
		"product_family":  "DGX Spark\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	system := detectSystem(root)
	if stringValue(system.Vendor) != "LENOVO" || stringValue(system.Model) != "ThinkStation PGX" || stringValue(system.Family) != "DGX Spark" {
		t.Fatalf("detectSystem() = %+v", system)
	}
}

func TestHardware_LinuxSystemPlaceholders(t *testing.T) {
	root := t.TempDir()
	for name, value := range map[string]string{
		"sys_vendor":      "To be filled by O.E.M.\n",
		"product_version": "System Product Name\n",
		"product_name":    "System Product Name\n",
		"product_family":  "Not Defined\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	system := detectSystem(root)
	if system.Vendor != nil || system.Model != nil || system.Family != nil {
		t.Fatalf("detectSystem() = %+v, want all fields nil", system)
	}
}

func TestHardware_LinuxSystemRevisionModel(t *testing.T) {
	tests := []struct {
		version string
		name    string
		want    string
	}{
		{version: "1.0\n", name: "Precision 5860 Tower", want: "Precision 5860 Tower"},
		{version: "0001\n", name: "OptiPlex 7010", want: "OptiPlex 7010"},
		{version: "Not Specified\n", name: "System X", want: "System X"},
		{version: "ThinkStation PGX\n", name: "30KL0004FC", want: "ThinkStation PGX"},
	}
	for _, test := range tests {
		t.Run(test.version, func(t *testing.T) {
			root := t.TempDir()
			for name, value := range map[string]string{"sys_vendor": "OEM\n", "product_version": test.version, "product_name": test.name + "\n"} {
				if err := os.WriteFile(filepath.Join(root, name), []byte(value), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			system := detectSystem(root)
			if stringValue(system.Model) != test.want {
				t.Fatalf("detectSystem().Model = %q, want %q", stringValue(system.Model), test.want)
			}
		})
	}
}

func TestHardware_LinuxSystemModelFallback(t *testing.T) {
	root := t.TempDir()
	for name, value := range map[string]string{
		"sys_vendor":      "NVIDIA\n",
		"product_version": "To be filled by O.E.M.\n",
		"product_name":    "DGX Spark\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	system := detectSystem(root)
	if stringValue(system.Vendor) != "NVIDIA" || stringValue(system.Model) != "DGX Spark" || system.Family != nil {
		t.Fatalf("detectSystem() = %+v, want model to fall back to product_name", system)
	}
}
