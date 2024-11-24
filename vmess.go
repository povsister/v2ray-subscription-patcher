package patcher

import (
	"encoding/json"
	"fmt"

	"github.com/tidwall/gjson"
)

type VmessConfig struct {
	V          string `json:"v"`
	ServerName string `json:"ps"`
	Addr       string `json:"add"`
	Port       int    `json:"port,string"`
	UUID       string `json:"id"`
	AlterId    int    `json:"aid,string"`
	Net        string `json:"net"`
	Type       string `json:"type"`
	Host       string `json:"host"`
	Path       string `json:"path"`
	TLS        string `json:"tls"`
}

func (m *SubItem) RetrieveVmessConf() error {
	if !gjson.ValidBytes(m.VmessRaw) {
		return fmt.Errorf("vmess payload is not a invalid json: %s", string(m.VmessRaw))
	}
	c := new(VmessConfig)
	err := json.Unmarshal(m.VmessRaw, c)
	if err != nil {
		return err
	}
	m.VmessConf = c
	return nil
}
