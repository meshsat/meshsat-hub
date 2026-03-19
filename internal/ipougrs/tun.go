package ipougrs

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/unix"
)

// TUNDevice wraps a Linux TUN device for reading and writing IP packets.
// Pure Go, no CGO — uses ioctl via golang.org/x/sys/unix.
type TUNDevice struct {
	file *os.File
	name string
	mtu  int
}

// ifreq is the Linux ifreq struct layout for TUNSETIFF ioctl.
type ifreq struct {
	name  [unix.IFNAMSIZ]byte
	flags uint16
	_     [22]byte // padding to match sizeof(struct ifreq)
}

// OpenTUN creates and configures a TUN device.
// Requires CAP_NET_ADMIN (or root).
func OpenTUN(name string, mtu int) (*TUNDevice, error) {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("ipougrs: open /dev/net/tun: %w", err)
	}

	var req ifreq
	copy(req.name[:], name)
	req.flags = unix.IFF_TUN | unix.IFF_NO_PI // TUN mode, no packet info header

	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(unix.TUNSETIFF), uintptr(unsafe.Pointer(&req))); errno != 0 {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("ipougrs: TUNSETIFF: %w", errno)
	}

	// Extract actual device name (kernel may have appended a number).
	actualName := string(req.name[:])
	for i, b := range req.name {
		if b == 0 {
			actualName = string(req.name[:i])
			break
		}
	}

	return &TUNDevice{
		file: os.NewFile(uintptr(fd), "/dev/net/tun"),
		name: actualName,
		mtu:  mtu,
	}, nil
}

// Name returns the TUN device name.
func (d *TUNDevice) Name() string { return d.name }

// MTU returns the configured MTU.
func (d *TUNDevice) MTU() int { return d.mtu }

// Read reads a single IP packet from the TUN device.
func (d *TUNDevice) Read(buf []byte) (int, error) {
	return d.file.Read(buf)
}

// Write writes an IP packet to the TUN device.
func (d *TUNDevice) Write(packet []byte) (int, error) {
	return d.file.Write(packet)
}

// Close closes the TUN device.
func (d *TUNDevice) Close() error {
	return d.file.Close()
}

// ConfigureAddress assigns an IP address to the TUN interface and brings it up.
// Uses `ip` command — requires CAP_NET_ADMIN.
func ConfigureAddress(devName, address, subnet string, mtu int) error {
	_, ipNet, err := net.ParseCIDR(subnet)
	if err != nil {
		return fmt.Errorf("ipougrs: parse subnet %q: %w", subnet, err)
	}
	ones, _ := ipNet.Mask.Size()
	cidr := fmt.Sprintf("%s/%d", address, ones)

	cmds := [][]string{
		{"ip", "addr", "add", cidr, "dev", devName},
		{"ip", "link", "set", devName, "mtu", fmt.Sprintf("%d", mtu)},
		{"ip", "link", "set", devName, "up"},
	}
	for _, args := range cmds {
		if err := exec.Command(args[0], args[1:]...).Run(); err != nil {
			return fmt.Errorf("ipougrs: %v: %w", args, err)
		}
	}
	return nil
}
