package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func envDuration(k string, d time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if dur, err := time.ParseDuration(v); err == nil {
			return dur
		}
	}
	return d
}

type Scaler struct {
	client      *kubernetes.Clientset
	namespace   string
	name        string
	wakeTimeout time.Duration

	mu       sync.Mutex
	active   atomic.Int64
	lastIdle atomic.Int64 // unix nanos
}

func (s *Scaler) markIdle() {
	s.lastIdle.Store(time.Now().UnixNano())
}

func (s *Scaler) setReplicas(ctx context.Context, replicas int32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	scale, err := s.client.AppsV1().StatefulSets(s.namespace).GetScale(ctx, s.name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get scale: %w", err)
	}
	if scale.Spec.Replicas == replicas {
		return nil
	}
	log.Printf("scaling statefulset/%s %d -> %d", s.name, scale.Spec.Replicas, replicas)
	scale.Spec.Replicas = replicas
	_, err = s.client.AppsV1().StatefulSets(s.namespace).UpdateScale(ctx, s.name, scale, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("update scale: %w", err)
	}
	return nil
}

func (s *Scaler) waitForBackend(ctx context.Context, addr string) error {
	deadline := time.Now().Add(s.wakeTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		d := net.Dialer{Timeout: 2 * time.Second}
		c, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = c.Close()
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("backend %s not ready within %s: %v", addr, s.wakeTimeout, lastErr)
}

func main() {
	listenAddr := envOr("LISTEN_ADDR", ":7777")
	backendAddr := envOr("BACKEND_ADDR", "terraria-backend.terraria.svc.cluster.local:7777")
	namespace := envOr("NAMESPACE", "terraria")
	stsName := envOr("STATEFULSET", "terraria")
	idleTimeout := envDuration("IDLE_TIMEOUT", 5*time.Minute)
	wakeTimeout := envDuration("WAKE_TIMEOUT", 120*time.Second)
	checkInterval := envDuration("CHECK_INTERVAL", 30*time.Second)

	cfg, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("in-cluster config: %v", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("clientset: %v", err)
	}

	s := &Scaler{
		client:      cs,
		namespace:   namespace,
		name:        stsName,
		wakeTimeout: wakeTimeout,
	}
	s.markIdle()

	// Idle watcher: when no active connections and idle window elapsed, scale to 0.
	go func() {
		t := time.NewTicker(checkInterval)
		defer t.Stop()
		for range t.C {
			if s.active.Load() != 0 {
				continue
			}
			idleSince := time.Unix(0, s.lastIdle.Load())
			if time.Since(idleSince) < idleTimeout {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := s.setReplicas(ctx, 0); err != nil {
				log.Printf("idle scale-down failed: %v", err)
			}
			cancel()
		}
	}()

	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatalf("listen %s: %v", listenAddr, err)
	}
	log.Printf("terraria wake-proxy: listen=%s backend=%s sts=%s/%s idle=%s wake=%s",
		listenAddr, backendAddr, namespace, stsName, idleTimeout, wakeTimeout)

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			continue
		}
		go handle(conn, s, backendAddr)
	}
}

func handle(client net.Conn, s *Scaler, backendAddr string) {
	defer client.Close()
	remote := client.RemoteAddr().String()
	log.Printf("conn open from %s", remote)

	s.active.Add(1)
	defer func() {
		if n := s.active.Add(-1); n == 0 {
			s.markIdle()
		}
		log.Printf("conn closed from %s", remote)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), s.wakeTimeout+10*time.Second)
	defer cancel()

	if err := s.setReplicas(ctx, 1); err != nil {
		log.Printf("scale up failed for %s: %v", remote, err)
		return
	}
	if err := s.waitForBackend(ctx, backendAddr); err != nil {
		log.Printf("wake failed for %s: %v", remote, err)
		return
	}

	backend, err := net.Dial("tcp", backendAddr)
	if err != nil {
		log.Printf("dial backend for %s: %v", remote, err)
		return
	}
	defer backend.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(backend, client)
		if tc, ok := backend.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(client, backend)
		if tc, ok := client.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()
	wg.Wait()
}
