package patcher

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type Subscription struct {
	Addr      string
	SubResult Base64Resp
	SubItems  []*SubItem
}

type JSONObject json.RawMessage

type Base64Resp []byte

type SubItem struct {
	Line      []byte
	Parsed    *url.URL
	VmessRaw  JSONObject
	VmessConf *VmessConfig
}

func NewSubscription(addr string) *Subscription {
	return &Subscription{Addr: addr}
}

func (s *Subscription) GetSubscription() error {
	slog.Info(fmt.Sprintf("Sending subscription request: %s", s.Addr))
	resp, err := http.Get(s.Addr)
	if err != nil {
		return err
	}
	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	s.SubResult = respBytes
	return nil
}

func (s *Subscription) ParseItems() error {
	if len(s.SubResult) <= 0 {
		return fmt.Errorf("no items in subscription result")
	}
	slog.Info("Parsing subscription result ...")
	buf := make([]byte, base64.StdEncoding.DecodedLen(len(s.SubResult)))
	n, err := base64.StdEncoding.Decode(buf, s.SubResult)
	if err != nil {
		return err
	}
	data := buf[:n]
	scaner := bufio.NewScanner(bytes.NewReader(data))
	scaner.Split(bufio.ScanLines)
	for scaner.Scan() {
		if len(scaner.Bytes()) <= 0 {
			continue
		}
		item := &SubItem{
			Line: scaner.Bytes(),
		}
		err = item.parseItem()
		if err != nil {
			return err
		}
		s.SubItems = append(s.SubItems, item)
	}
	slog.Info(fmt.Sprintf("Finished parsing subscription result: got %d subscription items", len(s.SubItems)))
	return nil
}

func (m *SubItem) ID() string {
	switch {
	case m.VmessRaw != nil:
		return m.VmessConf.Addr + ":" + strconv.Itoa(m.VmessConf.Port)
	}
	return string(m.Line)
}

func (m *SubItem) parseItem() error {
	u, err := url.Parse(string(m.Line))
	if err != nil {
		return err
	}
	m.Parsed = u
	if strings.EqualFold(m.Parsed.Scheme, "vmess") {
		// vmess://
		vmessPayload := m.Line[5+3:]
		buf := make([]byte, base64.StdEncoding.DecodedLen(len(vmessPayload)))
		n, err := base64.StdEncoding.Decode(buf, vmessPayload)
		if err != nil {
			return err
		}
		m.VmessRaw = buf[:n]
		err = m.RetrieveVmessConf()
		if err != nil {
			return err
		}
	}

	return nil
}
