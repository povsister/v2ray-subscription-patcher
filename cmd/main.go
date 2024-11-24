package main

import (
	"flag"

	patcher "github.com/povsister/v2ray-subscription-patcher"
)

var (
	subAddr string
)

func init() {
	flag.StringVar(&subAddr, "sub", "", "url address of v2ray subscription")
}

func main() {
	flag.Parse()
	if len(subAddr) <= 0 {
		panic("url address of v2ray subscription is empty")
	}
	s := patcher.NewSubscription(subAddr)
	err := s.GetSubscription()
	if err != nil {
		panic(err)
	}
	err = s.ParseItems()
	if err != nil {
		panic(err)
	}

	p := patcher.NewPatcher()
	err = p.ReadPrevConfig()
	if err != nil {
		panic(err)
	}
	err = p.ApplyPatchFromSubscription(s)
	if err != nil {
		panic(err)
	}
}
