package vif

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"

	"golang.org/x/sys/windows"
	"golang.zx2c4.com/wireguard/tun"
	"golang.zx2c4.com/wireguard/windows/tunnel/winipcfg"

	"github.com/datawire/dlib/derror"
	"github.com/datawire/dlib/dlog"
	"github.com/telepresenceio/telepresence/v2/pkg/proc"
	"github.com/telepresenceio/telepresence/v2/pkg/shellquote"
	"github.com/telepresenceio/telepresence/v2/pkg/vif/buffer"
)

// This nativeDevice will require that wintun.dll is available to the loader.
// See: https://www.wintun.net/ for more info.
type nativeDevice struct {
	tun.Device
	name           string
	dns            net.IP
	interfaceIndex int32
}

func openTun(ctx context.Context) (td *nativeDevice, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = derror.PanicToError(r)
			dlog.Errorf(ctx, "%+v", err)
		}
	}()
	interfaceName := "tel0"
	td = &nativeDevice{}
	if td.Device, err = tun.CreateTUN(interfaceName, 0); err != nil {
		return nil, fmt.Errorf("failed to create TUN device: %w", err)
	}
	if td.name, err = td.Device.Name(); err != nil {
		return nil, fmt.Errorf("failed to get real name of TUN device: %w", err)
	}
	iface, err := td.getLUID().Interface()
	if err != nil {
		return nil, fmt.Errorf("failed to get interface for TUN device: %w", err)
	}
	td.interfaceIndex = int32(iface.InterfaceIndex)

	return td, nil
}

func (t *nativeDevice) Close() error {
	// The tun.NativeTun device has a closing mutex which is read locked during
	// a call to Read(). The read lock prevents a call to Close() to proceed
	// until Read() actually receives something. To resolve that "deadlock",
	// we call Close() in one goroutine to wait for the lock and write a bogus
	// message in another that will be returned by Read().
	closeCh := make(chan error)
	go func() {
		// first message is just to indicate that this goroutine has started
		closeCh <- nil
		closeCh <- t.Device.Close()
		close(closeCh)
	}()

	// Not 100%, but we can be fairly sure that Close() is
	// hanging on the lock, or at least will be by the time
	// the Read() returns
	<-closeCh

	// Send something to the TUN device so that the Read
	// unlocks the NativeTun.closing mutex and let the actual
	// Close call continue
	conn, err := net.Dial("udp", t.dns.String()+":53")
	if err == nil {
		_, _ = conn.Write([]byte("bogus"))
	}
	return <-closeCh
}

func (t *nativeDevice) getLUID() winipcfg.LUID {
	return winipcfg.LUID(t.Device.(*tun.NativeTun).LUID())
}

func (t *nativeDevice) index() int32 {
	return t.interfaceIndex
}

func addrFromIP(ip net.IP) netip.Addr {
	var addr netip.Addr
	if ip4 := ip.To4(); ip4 != nil {
		addr = netip.AddrFrom4(*(*[4]byte)(ip4))
	} else if ip16 := ip.To16(); ip16 != nil {
		addr = netip.AddrFrom16(*(*[16]byte)(ip16))
	}
	return addr
}

func prefixFromIPNet(subnet *net.IPNet) netip.Prefix {
	if subnet == nil {
		return netip.Prefix{}
	}
	ones, _ := subnet.Mask.Size()
	return netip.PrefixFrom(addrFromIP(subnet.IP), ones)
}

func (t *nativeDevice) addSubnet(_ context.Context, subnet *net.IPNet) error {
	return t.getLUID().AddIPAddress(prefixFromIPNet(subnet))
}

func (t *nativeDevice) removeSubnet(_ context.Context, subnet *net.IPNet) error {
	return t.getLUID().DeleteIPAddress(prefixFromIPNet(subnet))
}

func (t *nativeDevice) setDNS(ctx context.Context, server net.IP, domains []string) (err error) {
	ipFamily := func(ip net.IP) winipcfg.AddressFamily {
		f := winipcfg.AddressFamily(windows.AF_INET6)
		if ip4 := ip.To4(); ip4 != nil {
			f = windows.AF_INET
		}
		return f
	}
	family := ipFamily(server)
	luid := t.getLUID()
	if t.dns != nil {
		if oldFamily := ipFamily(t.dns); oldFamily != family {
			_ = luid.FlushDNS(oldFamily)
		}
	}
	if err = luid.SetDNS(family, []netip.Addr{addrFromIP(server)}, domains); err != nil {
		return err
	}

	// On some systems (e.g. CircleCI but not josecv's windows box), SetDNS isn't enough to allow the domains to be resolved,
	// and the network adapter's domain has to be set explicitly.
	// It's actually way easier to do this via powershell than any system calls that can be run from go code
	domain := ""
	if len(domains) > 0 {
		// Quote the domain to prevent powershell injection
		domain = shellquote.ShellArgsString([]string{strings.TrimSuffix(domains[0], ".")})
	}
	// It's apparently well known that WMI queries can hang under various conditions, so we add a timeout here to prevent hanging the daemon
	// Fun fact: terminating the context that powershell is running in will not stop a hanging WMI call (!) perhaps because it is considered uninterruptible
	// For more on WMI queries hanging, see:
	//     * http://www.yusufozturk.info/windows-powershell/how-to-avoid-wmi-query-hangs-in-powershell.html
	//     * https://theolddogscriptingblog.wordpress.com/2012/05/11/wmi-hangs-and-how-to-avoid-them/
	//     * https://stackoverflow.com/questions/24294408/gwmi-query-hangs-powershell-script
	//     * http://use-powershell.blogspot.com/2018/03/get-wmiobject-hangs.html
	pshScript := fmt.Sprintf(`
$job = Get-WmiObject Win32_NetworkAdapterConfiguration -filter "interfaceindex='%d'" -AsJob | Wait-Job -Timeout 30
if ($job.State -ne 'Completed') {
	throw "timed out getting network adapter after 30 seconds."
}
$obj = $job | Receive-Job
$job = Invoke-WmiMethod -InputObject $obj -Name SetDNSDomain -ArgumentList "%s" -AsJob | Wait-Job -Timeout 30
if ($job.State -ne 'Completed') {
	throw "timed out setting network adapter DNS Domain after 30 seconds."
}
$job | Receive-Job
`, t.interfaceIndex, domain)
	cmd := proc.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", pshScript)
	cmd.DisableLogging = true // disable chatty logging
	dlog.Debugf(ctx, "Calling powershell's SetDNSDomain %q", domain)
	if err := cmd.Run(); err != nil {
		// Log the error, but don't actually fail on it: This is all just a fallback for SetDNS, so the domains might actually be working
		dlog.Errorf(ctx, "Failed to set NetworkAdapterConfiguration DNS Domain: %v. Will proceed, but namespace mapping might not be functional.", err)
	}
	t.dns = server
	return nil
}

func (t *nativeDevice) setMTU(int) error {
	return errors.New("not implemented")
}

func (t *nativeDevice) readPacket(into *buffer.Data) (int, error) {
	return t.Device.Read(into.Raw(), 0)
}

func (t *nativeDevice) writePacket(from *buffer.Data, offset int) (int, error) {
	return t.Device.Write(from.Raw(), offset)
}
