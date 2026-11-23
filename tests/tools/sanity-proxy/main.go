// Command sanity-proxy is a test-only TCP<->unix bridge for the csi-sanity
// node suite. It runs inside the running controller pod and forwards a pod
// network port to the controller's CSI unix socket, so a worker-pinned test
// pod can reach the running controller without exposing the socket or
// modifying the Deployment. It is never part of a production image.
package main

import (
	"flag"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:9977", "TCP listen address")
	unixSocket := flag.String("unix", "/csi/csi.sock", "unix socket to forward to")
	flag.Parse()

	if *unixSocket == "" {
		log.Fatal("sanity-proxy: -unix is required")
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("sanity-proxy: listen %s: %v", *listen, err)
	}
	log.Printf("sanity-proxy: forwarding %s -> unix:%s", ln.Addr(), *unixSocket)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		_ = ln.Close()
	}()

	var wg sync.WaitGroup
	for {
		conn, err := ln.Accept()
		if err != nil {
			break
		}
		wg.Add(1)
		go func(c net.Conn) {
			defer wg.Done()
			defer c.Close()
			upstream, err := net.Dial("unix", *unixSocket)
			if err != nil {
				log.Printf("sanity-proxy: dial unix: %v", err)
				return
			}
			defer upstream.Close()
			done := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(upstream, c); done <- struct{}{} }()
			go func() { _, _ = io.Copy(c, upstream); done <- struct{}{} }()
			<-done
		}(conn)
	}
	wg.Wait()
}
