package utils

import (
	"crypto/md5"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

var log = logrus.New()

func FletcherChecksum8(inp string) uint8 {
	var ckA, ckB uint8
	for i := 0; i < len(inp); i++ {
		ckA = (ckA + inp[i]) % 0xf
		ckB = (ckB + ckA) % 0xf
	}
	return (ckB << 4) | ckA
}

func ShortHostname() (shortName string, err error) {
	var hostname string

	if filePath, ok := os.LookupEnv("RUNTIMECFG_HOSTNAME_PATH"); ok {
		dat, err := ioutil.ReadFile(filePath)
		if err != nil {
			log.WithFields(logrus.Fields{
				"filePath": filePath,
			}).Error("Failed to read file")
			return "", err
		}
		hostname = strings.TrimSuffix(string(dat), "\n")
		log.WithFields(logrus.Fields{
			"hostname": hostname,
			"file":     filePath,
		}).Debug("Hostname retrieved from a file")
	} else {
		hostname, err = os.Hostname()
		if err != nil {
			panic(err)
		}
		log.WithFields(logrus.Fields{
			"hostname": hostname,
		}).Debug("Hostname retrieved from OS")
	}
	splitHostname := strings.SplitN(hostname, ".", 2)
	shortName = splitHostname[0]
	return shortName, err
}

func EtcdShortHostname() (shortName string, err error) {
	shortHostname, err := ShortHostname()
	if err != nil {
		panic(err)
	}
	if !strings.Contains(shortHostname, "master") {
		return "", err
	}
	etcdHostname := strings.Replace(shortHostname, "master", "etcd", 1)
	return etcdHostname, err
}

func GetEtcdSRVMembers(domain string) (srvs []*net.SRV, err error) {
	_, srvs, err = net.LookupSRV("etcd-server-ssl", "tcp", domain)
	if err != nil {
		return srvs, err
	}
	return srvs, err
}

func GetFirstAddr(host string) (string, error) {
	addrs, err := net.LookupHost(host)
	if err != nil {
		return "", err
	}
	return addrs[0], nil
}

func IsKubernetesHealthy(port uint16) (bool, error) {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	client := &http.Client{Transport: transport}
	resp, err := client.Get(fmt.Sprintf("https://127.0.0.1:%d/readyz", port))
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return false, err
	}

	return string(body) == "ok", nil
}

func AlarmStabilization(cur_alrm bool, cur_defect bool, consecutive_ctr uint8, on_threshold uint8, off_threshold uint8) (bool, uint8) {
	var new_alrm bool = cur_alrm
	var threshold uint8

	if cur_alrm != cur_defect {
		consecutive_ctr++
		if cur_alrm {
			threshold = off_threshold
		} else {
			threshold = on_threshold
		}
		if consecutive_ctr >= threshold {
			new_alrm = !cur_alrm
			consecutive_ctr = 0
		}
	} else {
		consecutive_ctr = 0
	}
	return new_alrm, consecutive_ctr
}

func GetFileMd5(filePath string) (string, error) {
	var returnMD5String string
	file, err := os.Open(filePath)
	if err != nil {
		return returnMD5String, err
	}
	defer file.Close()
	hash := md5.New()
	if _, err := io.Copy(hash, file); err != nil {
		return returnMD5String, err
	}
	hashInBytes := hash.Sum(nil)[:16]
	returnMD5String = hex.EncodeToString(hashInBytes)
	return returnMD5String, nil
}

// AddressFilter is a function type to filter addresses
type AddressFilter func(netlink.Addr) bool

// RouteFilter is a function type to filter routes
type RouteFilter func(netlink.Route) bool

func getAddrs() (addrMap map[netlink.Link][]netlink.Addr, err error) {
	nlHandle, err := netlink.NewHandle(unix.NETLINK_ROUTE)
	if err != nil {
		return nil, err
	}
	defer nlHandle.Delete()

	links, err := nlHandle.LinkList()
	if err != nil {
		return nil, err
	}

	addrMap = make(map[netlink.Link][]netlink.Addr)
	for _, link := range links {
		addresses, err := nlHandle.AddrList(link, netlink.FAMILY_ALL)
		if err != nil {
			return nil, err
		}
		for _, address := range addresses {
			if _, ok := addrMap[link]; ok {
				addrMap[link] = append(addrMap[link], address)
			} else {
				addrMap[link] = []netlink.Addr{address}
			}
		}
	}
	log.Debugf("retrieved Address map %+v", addrMap)
	return addrMap, nil
}

func getRouteMap() (routeMap map[int][]netlink.Route, err error) {
	nlHandle, err := netlink.NewHandle(unix.NETLINK_ROUTE)
	if err != nil {
		return nil, err
	}
	defer nlHandle.Delete()

	routes, err := nlHandle.RouteList(nil, netlink.FAMILY_V6)
	if err != nil {
		return nil, err
	}

	routeMap = make(map[int][]netlink.Route)
	for _, route := range routes {
		if route.Protocol != unix.RTPROT_RA {
			log.Debugf("Ignoring route non Router advertisement route %+v", route)
			continue
		}
		if _, ok := routeMap[route.LinkIndex]; ok {
			routeMap[route.LinkIndex] = append(routeMap[route.LinkIndex], route)
		} else {
			routeMap[route.LinkIndex] = []netlink.Route{route}
		}
	}

	log.Debugf("Retrieved IPv6 route map %+v", routeMap)

	return routeMap, nil
}

// NonDeprecatedAddress returns true if the address is IPv6 and has a preferred lifetime of 0
func NonDeprecatedAddress(address netlink.Addr) bool {
	return !(net.IPv6len == len(address.IP) && address.PreferedLft == 0)
}

// NonDefaultRoute returns whether the passed Route is the default
func NonDefaultRoute(route netlink.Route) bool {
	return route.Dst != nil
}

// AddressesRouting takes a slice of Virtual IPs and returns a slice of configured addresses in the current network namespace that directly route to those vips. You can optionally pass an AddressFilter and/or RouteFilter to further filter down which addresses are considered
func AddressesRouting(vips []net.IP, af AddressFilter, rf RouteFilter) ([]net.IP, error) {
	addrMap, err := getAddrs()
	if err != nil {
		return nil, err
	}

	var routeMap map[int][]netlink.Route
	matches := make([]net.IP, 0)
	for link, addresses := range addrMap {
		for _, address := range addresses {
			maskPrefix, maskBits := address.Mask.Size()
			if !af(address) {
				continue
			}
			if net.IPv6len == len(address.IP) && maskPrefix == maskBits {
				if routeMap == nil {
					routeMap, err = getRouteMap()
					if err != nil {
						return nil, err
					}
				}
				if routes, ok := routeMap[link.Attrs().Index]; ok {
					for _, route := range routes {
						if !rf(route) {
							continue
						}
						routePrefix, _ := route.Dst.Mask.Size()
						log.Infof("Checking route %+v (mask %s) for address %+v", route, route.Dst.Mask, address)
						if routePrefix != 0 {
							containmentNet := net.IPNet{IP: address.IP, Mask: route.Dst.Mask}
							for _, vip := range vips {
								log.Infof("Checking whether address %s with route %s contains VIP %s", address, route, vip)
								if containmentNet.Contains(vip) {
									log.Infof("Address %s with route %s contains VIP %s", address, route, vip)
									matches = append(matches, address.IP)
								}
							}
						}
					}
				}
			} else {
				for _, vip := range vips {
					log.Infof("Checking whether address %s contains VIP %s", address, vip)
					if address.Contains(vip) {
						log.Infof("Address %s contains VIP %s", address, vip)
						matches = append(matches, address.IP)
					}
				}
			}
		}

	}
	return matches, nil
}
