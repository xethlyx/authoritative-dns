package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/miekg/dns"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

var ANNOTATION_KEY = "ns/key"
var SUBDOMAIN_KEY = "ns/subdomain"

type Config struct {
	Fqdn string `json:"fqdn"`
	Key string `json:"key"`
	DefaultService string `json:"defaultService"`
	Port int `json:"port"`
}

var config Config

type ResolveInfo struct {
	forward bool
	a []string
	aaaa []string
}

var defaultRecord *ResolveInfo
var records = map[string]ResolveInfo{}

func isIPv6(address string) bool {
	return strings.Count(address, ":") >= 2
}

func isDefault(svc *v1.Service) bool {
	return svc.Namespace + "/" + svc.Name == config.DefaultService
}

func shouldTrack(svc *v1.Service) bool {
	return svc.Annotations[ANNOTATION_KEY] == config.Key || isDefault(svc)
}

func beginTrack(svc *v1.Service) {
	if isDefault(svc) {
		if svc.Spec.Type != v1.ServiceTypeLoadBalancer {
			slog.Error("default service must be a load balancer")
		} else {
			output := ResolveInfo{}

			for _, ip := range svc.Status.LoadBalancer.Ingress {
				if ip.Hostname != "" {
					slog.Warn("hostname load balancer ingress is not supported", "svc", fmt.Sprintf("%s/%s", svc.Namespace, svc.Name))
					continue
				}
				if ip.IP == "" {
					slog.Warn("missing IP in load balancer ingress", "svc", fmt.Sprintf("%s/%s", svc.Namespace, svc.Name))
					continue
				}

				if isIPv6(ip.IP) {
					output.aaaa = append(output.aaaa, ip.IP)
				} else {
					output.a = append(output.a, ip.IP)
				}
			}

			defaultRecord = &output
		}
		return
	}

	subdomain := svc.Name
	if overwrite, ok := svc.Annotations[SUBDOMAIN_KEY]; ok {
		subdomain = overwrite
	}

	switch svc.Spec.Type {
	case v1.ServiceTypeClusterIP:
		records[subdomain] = ResolveInfo{
			forward: true,
		}
		slog.Info("added entry (forwarded)", "subdomain", subdomain)
		return
	case v1.ServiceTypeLoadBalancer:
		output := ResolveInfo{}

		for _, ip := range svc.Status.LoadBalancer.Ingress {
			if ip.Hostname != "" {
				slog.Warn("hostname load balancer ingress is not supported", "svc", fmt.Sprintf("%s/%s", svc.Namespace, svc.Name))
				continue
			}
			if ip.IP == "" {
				slog.Warn("missing IP in load balancer ingress", "svc", fmt.Sprintf("%s/%s", svc.Namespace, svc.Name))
				continue
			}

			if isIPv6(ip.IP) {
				output.aaaa = append(output.a, ip.IP)
			} else {
				output.a = append(output.a, ip.IP)
			}
		}
		slog.Info("added entry", "subdomain", subdomain)
		records[subdomain] = output
	default:
		slog.Warn("unsupported load balancer type", "svc", fmt.Sprintf("%s/%s", svc.Namespace, svc.Name))
	}

}

func endTrack(svc *v1.Service) {
	if isDefault(svc) {
		defaultRecord = nil
	}

	subdomain := svc.Name
	if overwrite, ok := svc.Annotations[SUBDOMAIN_KEY]; ok {
		subdomain = overwrite
	}

	slog.Info("removed entry", "subdomain", subdomain)
	delete(records, subdomain)
}

func parseQuery(m *dns.Msg) {
	if defaultRecord == nil {
		m.SetRcode(m, dns.RcodeServerFailure)
		return
	}

	for _, q := range m.Question {
		var record ResolveInfo
		if q.Name == config.Fqdn+"." {
			record = *defaultRecord
	 	} else {
			subdomain := strings.TrimSuffix(q.Name, "."+config.Fqdn+".")
			if subdomain == q.Name {
				m.SetRcode(m, dns.RcodeNotAuth)
				return
			}

			var ok bool
			record, ok = records[subdomain]
			if !ok {
				m.SetRcode(m, dns.RcodeNameError)
				continue
			}
		}

		switch q.Qtype {
		case dns.TypeA:
			ips := record.a
			if record.forward {
				ips = defaultRecord.a
			}

			for _, ip := range ips {
				rr, err := dns.NewRR(fmt.Sprintf("%s A %s", q.Name, ip))
				if err != nil {
					slog.Warn("error while creating RR", "err", err)
					continue
				}
				m.Answer = append(m.Answer, rr)
			}
		case dns.TypeAAAA:
			ips := record.aaaa
			if record.forward {
				ips = defaultRecord.aaaa
			}

			for _, ip := range ips {
				rr, err := dns.NewRR(fmt.Sprintf("%s AAAA %s", q.Name, ip))
				if err != nil {
					slog.Warn("error while creating RR", "err", err)
					continue
				}
				m.Answer = append(m.Answer, rr)
			}
		}
	}
}

func handleDnsRequest(w dns.ResponseWriter, r *dns.Msg) {
	m := new(dns.Msg)
	m.SetReply(r)
	m.Compress = false
	m.Authoritative = true

	switch r.Opcode {
	case dns.OpcodeQuery:
		parseQuery(m)
	}

	w.WriteMsg(m)
}

func connectToCluster() *kubernetes.Clientset {
	var config *rest.Config

	config, err := rest.InClusterConfig()
	if err != nil {
		slog.Info("could not find cluster config, falling back to kubeconfig flag")

		var kubeconfig *string
		if home := homedir.HomeDir(); home != "" {
			kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
		} else {
			kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
		}
		flag.Parse()

		config, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
		if err != nil {
			panic(err.Error())
		}
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	return clientset
}

func main() {
	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	configLocation := flag.String("config", "./dns.json", "absolute path to the config file")
	f, err := os.ReadFile(*configLocation)
	if err != nil {
		slog.Error("could not read config", "err", err)
		os.Exit(1)
	}

	err = json.Unmarshal(f, &config)
	if err != nil {
		slog.Error("could not unmarshal config", "err", err)
		os.Exit(1)
	}

	slog.Info("initializing", "defaultService", config.DefaultService, "fqdn", config.Fqdn, "key", config.Key)

	clientset := connectToCluster()

	factory := informers.NewSharedInformerFactory(clientset, 1*time.Hour)
	serviceInformer := factory.Core().V1().Services().Informer()
	serviceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			svc := obj.(*v1.Service)
			if shouldTrack(svc) {
				beginTrack(svc)
			}
		},
		UpdateFunc: func(oldObj, newObj any) {
			oldSvc := oldObj.(*v1.Service)
			newSvc := newObj.(*v1.Service)

			if shouldTrack(oldSvc) == shouldTrack(newSvc) {
				if shouldTrack(newSvc) {
					endTrack(oldSvc)
					beginTrack(newSvc)
				}
				return
			}

			if shouldTrack(oldSvc) && !shouldTrack(newSvc) {
				endTrack(newSvc)
			} else {
				beginTrack(newSvc)
			}
		},
		DeleteFunc: func(obj any) {
			svc := obj.(*v1.Service)
			if shouldTrack(svc) {
				endTrack(svc)
			}
		},
	})

	slog.Info("starting informer")
	factory.Start(rootCtx.Done())

	slog.Info("waiting for cache sync")
	if !cache.WaitForCacheSync(rootCtx.Done(), serviceInformer.HasSynced) {
		slog.Error("shutdown while waiting for caches to sync")
		os.Exit(1)
	}

	slog.Info("cache synced")

	slog.Info("starting dns server", "port", config.Port)

	dnsShutdown := make(chan struct{})

	dns.HandleFunc(config.Fqdn+".", handleDnsRequest)
	tcpServer := &dns.Server{Addr: ":" + strconv.Itoa(config.Port), Net: "tcp"}
	udpServer := &dns.Server{Addr: ":" + strconv.Itoa(config.Port), Net: "udp"}
	go func() {
		err = tcpServer.ListenAndServe()
		if err != nil {
			slog.Error("tcp dns server crashed", "err", err.Error())
			os.Exit(1)
		}

		slog.Info("stopped tcp dns server")
		dnsShutdown<-struct{}{}
	}()
	go func() {
		err = udpServer.ListenAndServe()
		if err != nil {
			slog.Error("udp dns server crashed", "err", err.Error())
			os.Exit(1)
		}

		slog.Info("stopped udp dns server")
		dnsShutdown<-struct{}{}
	}()

	<-rootCtx.Done()
	if err := tcpServer.Shutdown(); err != nil {
		slog.Error("failed to shutdown tcp server", "err", err)
	} else {
		<-dnsShutdown
	}
	if err := udpServer.Shutdown(); err != nil {
		slog.Error("failed to shutdown udp server", "err", err)
	} else {
		<-dnsShutdown
	}

	slog.Info("gracefully exited")
}