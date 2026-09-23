package main

import (
	"net"

	genericapiserver "k8s.io/apiserver/pkg/server"
	genericoptions "k8s.io/apiserver/pkg/server/options"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/component-base/logs"
	logsapi "k8s.io/component-base/logs/api/v1"
	netutils "k8s.io/utils/net"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/apiserver"
)

type options struct {
	SecureServing  *genericoptions.SecureServingOptionsWithLoopback
	Authentication *genericoptions.DelegatingAuthenticationOptions
	Authorization  *genericoptions.DelegatingAuthorizationOptions
	Logging        *logs.Options
	Harbor         *harborOptions
	Replication    *replicationOptions
	Kubeconfig     string
}

func newOptions() *options {
	o := &options{
		SecureServing:  genericoptions.NewSecureServingOptions().WithLoopback(),
		Authentication: genericoptions.NewDelegatingAuthenticationOptions(),
		Authorization:  genericoptions.NewDelegatingAuthorizationOptions(),
		Logging:        logs.NewOptions(),
		Harbor:         &harborOptions{},
		Replication:    &replicationOptions{},
	}
	o.SecureServing.BindPort = 6443
	o.SecureServing.ServerCert.CertDirectory = ""
	return o
}

func (o *options) flags() cliflag.NamedFlagSets {
	var fss cliflag.NamedFlagSets
	o.SecureServing.AddFlags(fss.FlagSet("secure serving"))
	o.Authentication.AddFlags(fss.FlagSet("authentication"))
	o.Authorization.AddFlags(fss.FlagSet("authorization"))
	logsapi.AddFlags(o.Logging, fss.FlagSet("logging"))
	o.Harbor.addFlags(fss.FlagSet("harbor"))
	o.Replication.addFlags(fss.FlagSet("replication"))
	fss.FlagSet("namespaces").StringVar(&o.Kubeconfig, "kubeconfig", o.Kubeconfig, "Kubeconfig for reading namespaces. Defaults to the in-cluster configuration.")
	return fss
}

func (o *options) config() (*genericapiserver.Config, error) {
	if err := o.SecureServing.MaybeDefaultWithSelfSignedCerts("localhost", nil, []net.IP{netutils.ParseIPSloppy("127.0.0.1")}); err != nil {
		return nil, err
	}
	c := apiserver.NewConfig()
	if err := o.SecureServing.ApplyTo(&c.SecureServing, &c.LoopbackClientConfig); err != nil {
		return nil, err
	}
	if err := o.Authentication.ApplyTo(&c.Authentication, c.SecureServing, c.OpenAPIConfig); err != nil {
		return nil, err
	}
	if err := o.Authorization.ApplyTo(&c.Authorization); err != nil {
		return nil, err
	}
	return c, nil
}
