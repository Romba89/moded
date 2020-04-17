package main

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"time"

	"www.bamsoftware.com/git/dnstt.git/turbotunnel"
)

type HTTPPacketConn struct {
	urlString string
	client    *http.Client
	*turbotunnel.QueuePacketConn
}

func NewHTTPPacketConn(urlString string, numSenders int) (*HTTPPacketConn, error) {
	c := &HTTPPacketConn{
		urlString: urlString,
		client: &http.Client{
			Timeout: 1 * time.Minute,
		},
		QueuePacketConn: turbotunnel.NewQueuePacketConn(dummyAddr{}, idleTimeout),
	}
	for i := 0; i < numSenders; i++ {
		go func() {
			for p := range c.QueuePacketConn.OutgoingQueue(dummyAddr{}) {
				err := c.send(p)
				if err != nil {
					log.Printf("sender thread: %v", err)
				}
			}
		}()
	}
	return c, nil
}

// send sends a single packet in an HTTP request.
func (c *HTTPPacketConn) send(p []byte) error {
	req, err := http.NewRequest("POST", c.urlString, bytes.NewReader(p))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/dns-message")
	req.Header.Set("Content-Type", "application/dns-message")
	req.Header.Set("User-Agent", "") // Disable default "Go-http-client/1.1".
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		if ct := resp.Header.Get("Content-Type"); ct != "application/dns-message" {
			return fmt.Errorf("unknown HTTP response Content-Type %+q", ct)
		}
		body, err := ioutil.ReadAll(io.LimitReader(resp.Body, 64000))
		if err == nil {
			c.QueuePacketConn.QueueIncoming(body, dummyAddr{})
		}
		// Ignore err != nil; don't report an error if we at least
		// managed to send.
	default:
		return fmt.Errorf("unknown HTTP response status %+q", resp.Status)
	}
	return nil
}

func (c *HTTPPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	// TODO delay to Retry-After
	// Ignore addr.
	return c.QueuePacketConn.WriteTo(p, dummyAddr{})
}
