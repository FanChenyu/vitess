/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package servenv

import (
	"crypto/tls"
	"flag"
	"fmt"
	"math"
	"net"
	"time"

	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"vitess.io/vitess/go/trace"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"

	"context"

	"vitess.io/vitess/go/vt/grpccommon"
	"vitess.io/vitess/go/vt/grpcoptionaltls"
	"vitess.io/vitess/go/vt/log"
	"vitess.io/vitess/go/vt/vttls"
)

// This file handles gRPC server, on its own port.
// Clients register servers, based on service map:
//
// servenv.RegisterGRPCFlags()
// servenv.OnRun(func() {
//   if servenv.GRPCCheckServiceMap("XXX") {
//     pb.RegisterXXX(servenv.GRPCServer, XXX)
//   }
// }
//
// Note servenv.GRPCServer can only be used in servenv.OnRun,
// and not before, as it is initialized right before calling OnRun.
var (
	// GRPCPort is the port to listen on for gRPC. If not set or zero, don't listen.
	GRPCPort = flag.Int("grpc_port", 0, "Port to listen on for gRPC calls")

	// GRPCCert is the cert to use if TLS is enabled
	GRPCCert = flag.String("grpc_cert", "", "server certificate to use for gRPC connections, requires grpc_key, enables TLS")

	// GRPCKey is the key to use if TLS is enabled
	GRPCKey = flag.String("grpc_key", "", "server private key to use for gRPC connections, requires grpc_cert, enables TLS")

	// GRPCCA is the CA to use if TLS is enabled
	GRPCCA = flag.String("grpc_ca", "", "server CA to use for gRPC connections, requires TLS, and enforces client certificate check")

	// GRPCCRL is the CRL (Certificate Revocation List) to use if TLS is enabled
	GRPCCRL = flag.String("grpc_crl", "", "path to a certificate revocation list in PEM format, client certificates will be further verified against this file during TLS handshake")

	GRPCEnableOptionalTLS = flag.Bool("grpc_enable_optional_tls", false, "enable optional TLS mode when a server accepts both TLS and plain-text connections on the same port")

	// GRPCServerCA if specified will combine server cert and server CA
	GRPCServerCA = flag.String("grpc_server_ca", "", "path to server CA in PEM format, which will be combine with server cert, return full certificate chain to clients")

	// GRPCAuth which auth plugin to use (at the moment now only static is supported)
	GRPCAuth = flag.String("grpc_auth_mode", "", "Which auth plugin implementation to use (eg: static)")

	// GRPCServer is the global server to serve gRPC.
	GRPCServer *grpc.Server

	// GRPCMaxConnectionAge is the maximum age of a client connection, before GoAway is sent.
	// This is useful for L4 loadbalancing to ensure rebalancing after scaling.
	GRPCMaxConnectionAge = flag.Duration("grpc_max_connection_age", time.Duration(math.MaxInt64), "Maximum age of a client connection before GoAway is sent.")

	// GRPCMaxConnectionAgeGrace is an additional grace period after GRPCMaxConnectionAge, after which
	// connections are forcibly closed.
	GRPCMaxConnectionAgeGrace = flag.Duration("grpc_max_connection_age_grace", time.Duration(math.MaxInt64), "Additional grace period after grpc_max_connection_age, after which connections are forcibly closed.")

	// GRPCInitialConnWindowSize ServerOption that sets window size for a connection.
	// The lower bound for window size is 64K and any value smaller than that will be ignored.
	GRPCInitialConnWindowSize = flag.Int("grpc_server_initial_conn_window_size", 0, "gRPC server initial connection window size")

	// GRPCInitialWindowSize ServerOption that sets window size for stream.
	// The lower bound for window size is 64K and any value smaller than that will be ignored.
	GRPCInitialWindowSize = flag.Int("grpc_server_initial_window_size", 0, "gRPC server initial window size")

	// EnforcementPolicy MinTime that sets the keepalive enforcement policy on the server.
	// This is the minimum amount of time a client should wait before sending a keepalive ping.
	GRPCKeepAliveEnforcementPolicyMinTime = flag.Duration("grpc_server_keepalive_enforcement_policy_min_time", 10*time.Second, "gRPC server minimum keepalive time")

	// EnforcementPolicy PermitWithoutStream - If true, server allows keepalive pings
	// even when there are no active streams (RPCs). If false, and client sends ping when
	// there are no active streams, server will send GOAWAY and close the connection.
	GRPCKeepAliveEnforcementPolicyPermitWithoutStream = flag.Bool("grpc_server_keepalive_enforcement_policy_permit_without_stream", false, "gRPC server permit client keepalive pings even when there are no active streams (RPCs)")

	authPlugin Authenticator
)

// isGRPCEnabled returns true if gRPC server is set
func isGRPCEnabled() bool {
	if GRPCPort != nil && *GRPCPort != 0 {
		return true
	}

	if SocketFile != nil && *SocketFile != "" {
		return true
	}

	return false
}

// createGRPCServer create the gRPC server we will be using.
// It has to be called after flags are parsed, but before
// services register themselves.
func createGRPCServer() {
	// skip if not registered
	if !isGRPCEnabled() {
		log.Infof("Skipping gRPC server creation")
		return
	}

	grpccommon.EnableTracingOpt()

	var opts []grpc.ServerOption
	if GRPCPort != nil && *GRPCCert != "" && *GRPCKey != "" {
		config, err := vttls.ServerConfig(*GRPCCert, *GRPCKey, *GRPCCA, *GRPCCRL, *GRPCServerCA, tls.VersionTLS12)
		if err != nil {
			log.Exitf("Failed to log gRPC cert/key/ca: %v", err)
		}

		// create the creds server options
		creds := credentials.NewTLS(config)
		if *GRPCEnableOptionalTLS {
			log.Warning("Optional TLS is active. Plain-text connections will be accepted")
			creds = grpcoptionaltls.New(creds)
		}
		opts = []grpc.ServerOption{grpc.Creds(creds)}
	}
	// Override the default max message size for both send and receive
	// (which is 4 MiB in gRPC 1.0.0).
	// Large messages can occur when users try to insert or fetch very big
	// rows. If they hit the limit, they'll see the following error:
	// grpc: received message length XXXXXXX exceeding the max size 4194304
	// Note: For gRPC 1.0.0 it's sufficient to set the limit on the server only
	// because it's not enforced on the client side.
	log.Infof("Setting grpc max message size to %d", *grpccommon.MaxMessageSize)
	opts = append(opts, grpc.MaxRecvMsgSize(*grpccommon.MaxMessageSize))
	opts = append(opts, grpc.MaxSendMsgSize(*grpccommon.MaxMessageSize))

	if *GRPCInitialConnWindowSize != 0 {
		log.Infof("Setting grpc server initial conn window size to %d", int32(*GRPCInitialConnWindowSize))
		opts = append(opts, grpc.InitialConnWindowSize(int32(*GRPCInitialConnWindowSize)))
	}

	if *GRPCInitialWindowSize != 0 {
		log.Infof("Setting grpc server initial window size to %d", int32(*GRPCInitialWindowSize))
		opts = append(opts, grpc.InitialWindowSize(int32(*GRPCInitialWindowSize)))
	}

	ep := keepalive.EnforcementPolicy{
		MinTime:             *GRPCKeepAliveEnforcementPolicyMinTime,
		PermitWithoutStream: *GRPCKeepAliveEnforcementPolicyPermitWithoutStream,
	}
	opts = append(opts, grpc.KeepaliveEnforcementPolicy(ep))

	if GRPCMaxConnectionAge != nil {
		ka := keepalive.ServerParameters{
			MaxConnectionAge: *GRPCMaxConnectionAge,
		}
		if GRPCMaxConnectionAgeGrace != nil {
			ka.MaxConnectionAgeGrace = *GRPCMaxConnectionAgeGrace
		}
		opts = append(opts, grpc.KeepaliveParams(ka))
	}

	opts = append(opts, interceptors()...)

	GRPCServer = grpc.NewServer(opts...)
}

// We can only set a ServerInterceptor once, so we chain multiple interceptors into one
func interceptors() []grpc.ServerOption {
	interceptors := &serverInterceptorBuilder{}

	if *GRPCAuth != "" {
		log.Infof("enabling auth plugin %v", *GRPCAuth)
		pluginInitializer := GetAuthenticator(*GRPCAuth)
		authPluginImpl, err := pluginInitializer()
		if err != nil {
			log.Fatalf("Failed to load auth plugin: %v", err)
		}
		authPlugin = authPluginImpl
		interceptors.Add(authenticatingStreamInterceptor, authenticatingUnaryInterceptor)
	}

	if *grpccommon.EnableGRPCPrometheus {
		interceptors.Add(grpc_prometheus.StreamServerInterceptor, grpc_prometheus.UnaryServerInterceptor)
	}

	trace.AddGrpcServerOptions(interceptors.Add)

	return interceptors.Build()
}

func serveGRPC() {
	if *grpccommon.EnableGRPCPrometheus {
		grpc_prometheus.Register(GRPCServer)
		grpc_prometheus.EnableHandlingTimeHistogram()
	}
	// skip if not registered
	if GRPCPort == nil || *GRPCPort == 0 {
		return
	}

	// register reflection to support list calls :)
	reflection.Register(GRPCServer)

	// register health service to support health checks
	healthServer := health.NewServer()
	healthpb.RegisterHealthServer(GRPCServer, healthServer)

	for service := range GRPCServer.GetServiceInfo() {
		healthServer.SetServingStatus(service, healthpb.HealthCheckResponse_SERVING)
	}

	// listen on the port
	log.Infof("Listening for gRPC calls on port %v", *GRPCPort)
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", *GRPCPort))
	if err != nil {
		log.Exitf("Cannot listen on port %v for gRPC: %v", *GRPCPort, err)
	}

	// and serve on it
	// NOTE: Before we call Serve(), all services must have registered themselves
	//       with "GRPCServer". This is the case because go/vt/servenv/run.go
	//       runs all OnRun() hooks after createGRPCServer() and before
	//       serveGRPC(). If this was not the case, the binary would crash with
	//       the error "grpc: Server.RegisterService after Server.Serve".
	go func() {
		err := GRPCServer.Serve(listener)
		if err != nil {
			log.Exitf("Failed to start grpc server: %v", err)
		}
	}()

	OnTermSync(func() {
		log.Info("Initiated graceful stop of gRPC server")
		GRPCServer.GracefulStop()
		log.Info("gRPC server stopped")
	})
}

// GRPCCheckServiceMap returns if we should register a gRPC service
// (and also logs how to enable / disable it)
func GRPCCheckServiceMap(name string) bool {
	// Silently fail individual services if gRPC is not enabled in
	// the first place (either on a grpc port or on the socket file)
	if !isGRPCEnabled() {
		return false
	}

	// then check ServiceMap
	return CheckServiceMap("grpc", name)
}

func authenticatingStreamInterceptor(srv interface{}, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	newCtx, err := authPlugin.Authenticate(stream.Context(), info.FullMethod)

	if err != nil {
		return err
	}

	wrapped := WrapServerStream(stream)
	wrapped.WrappedContext = newCtx
	return handler(srv, wrapped)
}

func authenticatingUnaryInterceptor(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	newCtx, err := authPlugin.Authenticate(ctx, info.FullMethod)
	if err != nil {
		return nil, err
	}

	return handler(newCtx, req)
}

// WrappedServerStream is based on the service stream wrapper from: https://github.com/grpc-ecosystem/go-grpc-middleware
type WrappedServerStream struct {
	grpc.ServerStream
	WrappedContext context.Context
}

// Context returns the wrapper's WrappedContext, overwriting the nested grpc.ServerStream.Context()
func (w *WrappedServerStream) Context() context.Context {
	return w.WrappedContext
}

// WrapServerStream returns a ServerStream that has the ability to overwrite context.
func WrapServerStream(stream grpc.ServerStream) *WrappedServerStream {
	if existing, ok := stream.(*WrappedServerStream); ok {
		return existing
	}
	return &WrappedServerStream{ServerStream: stream, WrappedContext: stream.Context()}
}

// serverInterceptorBuilder chains together multiple ServerInterceptors
type serverInterceptorBuilder struct {
	streamInterceptors []grpc.StreamServerInterceptor
	unaryInterceptors  []grpc.UnaryServerInterceptor
}

// Add adds interceptors to the builder
func (collector *serverInterceptorBuilder) Add(s grpc.StreamServerInterceptor, u grpc.UnaryServerInterceptor) {
	collector.streamInterceptors = append(collector.streamInterceptors, s)
	collector.unaryInterceptors = append(collector.unaryInterceptors, u)
}

// AddUnary adds a single unary interceptor to the builder
func (collector *serverInterceptorBuilder) AddUnary(u grpc.UnaryServerInterceptor) {
	collector.unaryInterceptors = append(collector.unaryInterceptors, u)
}

// Build returns DialOptions to add to the grpc.Dial call
func (collector *serverInterceptorBuilder) Build() []grpc.ServerOption {
	log.Infof("Building interceptors with %d unary interceptors and %d stream interceptors", len(collector.unaryInterceptors), len(collector.streamInterceptors))
	switch len(collector.unaryInterceptors) + len(collector.streamInterceptors) {
	case 0:
		return []grpc.ServerOption{}
	default:
		return []grpc.ServerOption{
			grpc.UnaryInterceptor(grpc_middleware.ChainUnaryServer(collector.unaryInterceptors...)),
			grpc.StreamInterceptor(grpc_middleware.ChainStreamServer(collector.streamInterceptors...)),
		}
	}
}
