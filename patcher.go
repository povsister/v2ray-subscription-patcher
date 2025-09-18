package patcher

import (
	"bytes"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"strings"
	"unsafe"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var (
	v2rayConfigPath string
)

func init() {
	flag.StringVar(&v2rayConfigPath, "v2ray-config", "/usr/local/etc/v2ray/config.json", "v2ray jsonV4 config path")
}

type Patcher struct {
	V2RayConf JSONObject
	Output    JSONObject
	// addr:port
	VmessServers map[string]*SubItem

	dnsRtOutbounds          []string
	dnsRtBalancers          []string
	dnsRtAllRegionSuffix    map[string]struct{}
	dnsRtAllRegionSuffixSlc []string

	newOutbounds    []gjson.Result
	newBalancers    []gjson.Result
	fallbackMap     map[string]string
	lastFallbackTag string
	newObservers    []gjson.Result
	newRoutingRules []gjson.Result
}

func NewPatcher() *Patcher {
	p := &Patcher{}

	return p
}

func (p *Patcher) ReadPrevConfig() error {
	slog.Info(fmt.Sprintf("Reading v2ray config file: %s", v2rayConfigPath))
	f, err := os.ReadFile(v2rayConfigPath)
	if err != nil {
		return err
	}
	if gjson.ValidBytes(f) {
		return fmt.Errorf("v2ray config file is not a valid JSON file")
	}
	p.V2RayConf = f

	return nil
}

func (p *Patcher) ApplyPatchFromSubscription(sub *Subscription) (err error) {
	p.VmessServers = make(map[string]*SubItem, len(sub.SubItems))
	for _, item := range sub.SubItems {
		if item.VmessConf == nil {
			continue
		}
		p.VmessServers[item.ID()] = item
	}
	p.Output = make(JSONObject, len(p.V2RayConf))
	copy(p.Output, p.V2RayConf)

	// 切出来dns route关注的outboundTags或者balancerTags
	if err = p.retrieveDnsRtTags(); err != nil {
		return
	}
	// outbounds
	if err = p.retrieveOutbounds(sub); err != nil {
		return
	}
	// balancers
	if err = p.retrieveBalancers(); err != nil {
		return
	}
	// observers
	if err = p.retrieveObservatory(); err != nil {
		return
	}
	if err = p.retrieveRoutingRules(); err != nil {
		return
	}

	// 进行patch
	if err = p.prepareObservatoryAndBalancers(); err != nil {
		return
	}
	if err = p.prepareOutbounds(); err != nil {
		return
	}
	if err = p.prepareDnsRtConnTrackRoutingRules(); err != nil {
		return
	}
	err = p.writeOutPatchedResult()

	return
}

const (
	autoSetupBalancerPrefix = "balancer-proxy-"
	autoSetupOutboundPrefix = "proxy-"
	autoSetupObserverPrefix = "observatory-internet-proxy-"
)

func (p *Patcher) retrieveDnsRtTags() error {
	dnsCircuit := gjson.GetBytes(p.V2RayConf, "dnsCircuit")
	if !dnsCircuit.Exists() || !dnsCircuit.IsObject() {
		return fmt.Errorf("dnsCircuit config not found in v2ray config")
	}
	for _, f := range []struct {
		Field string
		Store func(tag ...string)
	}{
		{
			"outboundTags", func(tag ...string) {
			p.dnsRtOutbounds = append(p.dnsRtOutbounds, tag...)
		},
		},
		{
			"balancerTags", func(tag ...string) {
			p.dnsRtBalancers = append(p.dnsRtBalancers, tag...)
		},
		},
	} {
		tags := dnsCircuit.Get(f.Field)
		if tags.IsArray() {
			tags.ForEach(func(_, value gjson.Result) bool {
				f.Store(value.String())
				return true
			})
		} else {
			f.Store(strings.Split(tags.String(), ",")...)
		}
	}
	if len(p.dnsRtOutbounds) <= 0 && len(p.dnsRtBalancers) <= 0 {
		return fmt.Errorf("no dnsCircuit outboundTags or balancerTags found in v2ray config")
	}

	p.dnsRtAllRegionSuffix = make(map[string]struct{})
	for _, outTag := range p.dnsRtOutbounds {
		if !strings.HasPrefix(outTag, autoSetupOutboundPrefix) {
			continue
		}
		p.dnsRtAllRegionSuffix[strings.TrimPrefix(outTag, autoSetupOutboundPrefix)] = struct{}{}
	}
	for _, balaTag := range p.dnsRtBalancers {
		if !strings.HasPrefix(balaTag, autoSetupBalancerPrefix) {
			continue
		}
		p.dnsRtAllRegionSuffix[strings.TrimPrefix(balaTag, autoSetupBalancerPrefix)] = struct{}{}
	}
	p.dnsRtAllRegionSuffixSlc = make([]string, 0, len(p.dnsRtAllRegionSuffix))
	for region := range p.dnsRtAllRegionSuffix {
		p.dnsRtAllRegionSuffixSlc = append(p.dnsRtAllRegionSuffixSlc, region)
	}
	if len(p.dnsRtAllRegionSuffixSlc) > 0 {
		slog.Info(fmt.Sprintf("Retrieved %d region suffix from dnsCircuit outboundTags/balancerTags: %v",
			len(p.dnsRtAllRegionSuffixSlc), p.dnsRtAllRegionSuffixSlc))
	}
	return nil
}

const (
	pathOutbounds    = "outbounds"
	pathBalancers    = "routing.balancers"
	pathObservers    = "multiObservatory.observers"
	pathRoutingRules = "routing.rules"
)

func (p *Patcher) retrieveOutbounds(sub *Subscription) error {
	// 取出所有outbounds
	outbounds := gjson.GetBytes(p.V2RayConf, pathOutbounds)
	if !outbounds.Exists() || !outbounds.IsArray() {
		return fmt.Errorf(pathOutbounds + " is not an array or does not exist")
	}
	proxyNodes := make(map[string]gjson.Result, len(sub.SubItems)*2)
	var (
		firstOutbound gjson.Result
		newOutbounds  []gjson.Result
	)
	outbounds.ForEach(func(idx, outbound gjson.Result) (next bool) {
		if idx.Int() == 0 {
			firstOutbound = outbound
		}
		next = true
		tag := outbound.Get("tag")
		// 选出所有 proxy- 开头的outbounds
		if strings.HasPrefix(tag.String(), autoSetupOutboundPrefix) {
			proxyNodes[tag.String()] = outbound
		} else {
			newOutbounds = append(newOutbounds, outbound)
		}
		return
	})
	if firstOutbound.Get("tag").String() != newOutbounds[0].Get("tag").String() {
		slog.Warn(fmt.Sprintf("V2Ray default outbound changed from %q to %q. This may cause unexpected dispatch result.\nDouble check the config by yourself!!",
			firstOutbound.Get("tag").String(), newOutbounds[0].Get("tag").String()))
	}
	p.newOutbounds = newOutbounds
	slog.Info(fmt.Sprintf("Processing outbounds ... found&removed %d auto-generated and preserve %d customized outbounds",
		len(proxyNodes), len(p.newOutbounds)))
	return nil
}

func (p *Patcher) retrieveBalancers() error {
	balancers := gjson.GetBytes(p.V2RayConf, pathBalancers)
	if !balancers.Exists() {
		return nil
	}
	if !balancers.IsArray() {
		return fmt.Errorf(pathBalancers + " is not an array")
	}

	var (
		newBalancers []gjson.Result
		removedCnt   int
	)
	p.fallbackMap = make(map[string]string)
	balancers.ForEach(func(idx, balancer gjson.Result) (next bool) {
		next = true
		tag := balancer.Get("tag").String()
		// 留下所有不是balancer-proxy-开头的
		if !strings.HasPrefix(tag, autoSetupBalancerPrefix) {
			newBalancers = append(newBalancers, balancer)
			return
		}
		fallbackTag := balancer.Get("fallbackTag").String()
		p.fallbackMap[tag] = fallbackTag
		if len(fallbackTag) > 0 {
			p.lastFallbackTag = fallbackTag
		}
		removedCnt++
		return
	})
	p.newBalancers = newBalancers
	slog.Info(fmt.Sprintf("Processing balancers ... found&removed %d auto-generated and preserve %d customized balancers",
		removedCnt, len(p.newBalancers)))
	return nil
}

func (p *Patcher) retrieveObservatory() error {
	observatories := gjson.GetBytes(p.V2RayConf, pathObservers)
	if !observatories.Exists() {
		return nil
	}
	if !observatories.IsArray() {
		return fmt.Errorf(pathObservers + " is not an array")
	}

	var (
		newObservers []gjson.Result
		removedCnt   int
	)
	observatories.ForEach(func(idx, observer gjson.Result) (next bool) {
		next = true
		tag := observer.Get("tag").String()
		if !strings.HasPrefix(tag, autoSetupObserverPrefix) {
			newObservers = append(newObservers, observer)
		}
		removedCnt++
		return
	})
	p.newObservers = newObservers
	slog.Info(fmt.Sprintf("Processing observatories ... found&removed %d auto-generated and preserve %d customized observatories",
		removedCnt, len(p.newObservers)))
	return nil
}

const (
	routingRuleConnTrackSrcPrefix  = "dynamic-ipset:dnscircuit-conntrack-src-"
	routingRuleConnTrackDstPrefix  = "dynamic-ipset:dnscircuit-conntrack-dest-"
	routingRuleConnTrackDstDefault = "dynamic-ipset:dnscircuit-dest-default"
)

func (p *Patcher) retrieveRoutingRules() error {
	rules := gjson.GetBytes(p.V2RayConf, pathRoutingRules)
	if !rules.Exists() {
		return nil
	}
	if !rules.IsArray() {
		return fmt.Errorf(pathRoutingRules + " is not an array")
	}
	var (
		newRules   []gjson.Result
		removedCnt int
	)
	rules.ForEach(func(idx, rule gjson.Result) (next bool) {
		next = true
		rType := rule.Get("type").String()
		if rType != "field" {
			newRules = append(newRules, rule)
			return
		}
		src, dst := rule.Get("source"), rule.Get("ip")
		if !src.Exists() || !dst.Exists() {
			newRules = append(newRules, rule)
			return
		}
		if !strings.HasPrefix(src.String(), routingRuleConnTrackSrcPrefix) ||
			!strings.HasPrefix(dst.String(), routingRuleConnTrackDstPrefix) {
			newRules = append(newRules, rule)
			return
		}
		outTag, balaTag := rule.Get("outboundTag"), rule.Get("balancerTag")
		if outTag.Exists() {
			if strings.HasPrefix(outTag.String(), autoSetupOutboundPrefix) {
				removedCnt++
				return
			}
		}
		if balaTag.Exists() {
			if strings.HasPrefix(balaTag.String(), autoSetupBalancerPrefix) {
				removedCnt++
				return
			}
		}
		return
	})
	p.newRoutingRules = newRules
	slog.Info(fmt.Sprintf("Processing routingRules ... found&removed %d auto-generated and preserve %d customized routingRules",
		removedCnt, len(p.newRoutingRules)))
	return nil
}

func (p *Patcher) prepareObservatoryAndBalancers() error {
	if len(p.dnsRtBalancers) <= 0 {
		return nil
	}
	var (
		addedCnt int
	)
	for _, balancerTag := range p.dnsRtBalancers {
		if !strings.HasPrefix(balancerTag, autoSetupBalancerPrefix) {
			continue
		}
		regionSuffix := strings.TrimPrefix(balancerTag, autoSetupBalancerPrefix)
		suffixSplit := strings.Split(regionSuffix, "|")
		sls := make([]string, 0, len(suffixSplit))
		for _, suffix := range suffixSplit {
			sls = append(sls, fmt.Sprintf("%s%q:", autoSetupOutboundPrefix, suffix))
		}
		outBoundSelector := strings.Join(sls, ", ")
		// observatory
		p.newObservers = append(p.newObservers,
			gjson.Parse(fmt.Sprintf(`      { // Auto-generated from dnsCircuit.balancerTag = %s
        "type": "default",
        "tag": "%s",
        "settings": {
          "subjectSelector": [%s],
          "probeURL": "https://www.cloudflarestatus.com/api/v2/status.json",
          "probeInterval": "60s"
        }
      }`, balancerTag, autoSetupObserverPrefix+regionSuffix, outBoundSelector)))
		// balancers
		fallbackTag := p.fallbackMap[balancerTag]
		if len(fallbackTag) <= 0 {
			fallbackTag = p.lastFallbackTag
		}
		addedCnt++
		p.newBalancers = append(p.newBalancers,
			gjson.Parse(fmt.Sprintf(`      { // Auto-Generated from dnsCircuit.balancerTag = %s
        "tag": "%s",
        "selector": [%s],
        "strategy": {
          "type": "leastping",
          "settings": {
            "observerTag": "%s"
          }
        },
        "fallbackTag": "%s"
      }`, balancerTag, balancerTag, outBoundSelector,
				autoSetupObserverPrefix+regionSuffix, fallbackTag)))
	}
	if addedCnt > 0 {
		slog.Info(fmt.Sprintf("Preparing new observatories and balancers ... added %d auto-generated items", addedCnt))
	}
	return nil
}

var suffixTrimer = regexp.MustCompile(`\s*\([^)]*\)\s*`)

func (p *Patcher) prepareOutbounds() (err error) {
	rgx, err := regexp.Compile(strings.Join(p.dnsRtAllRegionSuffixSlc, "|"))
	if err != nil {
		return fmt.Errorf("failed to complie outbound matcher rgx: %w", err)
	}
	var (
		addedCnt int
	)
	for subId, subItem := range p.VmessServers {
		//tldPlus1, err := publicsuffix.EffectiveTLDPlusOne(subItem.VmessConf.Addr)
		//if err != nil {
		//	slog.Warn(fmt.Sprintf("invalid domain found in subscription result: %s", string(subItem.VmessRaw)))
		//	continue
		//}
		//itemAddrId := strings.TrimSuffix(subItem.VmessConf.Addr, tldPlus1)
		//itemAddrId = strings.TrimSuffix(itemAddrId, ".")
		serverName := strings.ToLower(strings.ReplaceAll(suffixTrimer.ReplaceAllString(subItem.VmessConf.ServerName, ""), " ", "-"))
		if m := rgx.FindAllString(serverName, -1); len(m) > 0 {
			if len(m) > 1 {
				slog.Warn(fmt.Sprintf("Vmess server %s matches more than one pattern from dnsCircuit balancerTags/outboundTags(%v). skipped.",
					subId, m))
				continue
			}
			addedCnt++
			p.newOutbounds = append(p.newOutbounds,
				gjson.Parse(fmt.Sprintf(`    {
      "tag": "%s", // Auto-Generated from %s
      "protocol": "vmess",
      "settings": {
        "vnext": [{
          "address": "%s",
          "port": %v,
          "users": [
            {
              "id": "%s",
              "alterId": %v
            }
          ]
        }]
      },
      "streamSettings": {
        "network": "tcp",
        "tcpSettings": {
          "header": {
            "type": "none"
          }
        },
        "security": "none",
        "sockopt": {
          "mark": 255
        }
      },
      "mux": {
        "enabled": false
      }
    }`, autoSetupOutboundPrefix+m[0]+":"+serverName+fmt.Sprintf("-p%d", subItem.VmessConf.Port),
					subId,
					subItem.VmessConf.Addr, subItem.VmessConf.Port, subItem.VmessConf.UUID, subItem.VmessConf.AlterId)))
		}
	}
	if addedCnt > 0 {
		slog.Info(fmt.Sprintf("Preparing new outbounds ... added %d auto-generated items", addedCnt))
	}
	return nil
}

func (p *Patcher) prepareDnsRtConnTrackRoutingRules() error {
	var (
		finalRoutingRules []gjson.Result
		processedOutTags  = make(map[string]bool)
		processedBalaTags = make(map[string]bool)
		addedCnt          int
	)
	for i := len(p.newRoutingRules) - 1; i >= 0; i-- {
		rule := p.newRoutingRules[i]
		outTag, balaTag := rule.Get("outboundTag"), rule.Get("balancerTag")
		var (
			setTag       func([]byte, string) ([]byte, error)
			outName      string
			regionSuffix string
		)
		src, dst := rule.Get("source"), rule.Get("ip")
		// conn-track 默认路由 不处理
		if dst.Exists() && dst.String() == routingRuleConnTrackDstDefault &&
			(!src.Exists() || len(src.String()) <= 0) {
			finalRoutingRules = slices.Insert(finalRoutingRules, 0, rule)
			continue
		}
		// 常用兜底写法，不处理
		if !src.Exists() && !dst.Exists() {
			if rule.Get("network").String() == "tcp,udp" {
				finalRoutingRules = slices.Insert(finalRoutingRules, 0, rule)
				continue
			}
		}
		// 自动添加 conn-track rule
		if outTag.Exists() {
			regionSuffix = strings.TrimPrefix(outTag.String(), autoSetupOutboundPrefix)
			_, ok := p.dnsRtAllRegionSuffix[regionSuffix]
			outName = outTag.String()
			if !strings.HasPrefix(outName, autoSetupOutboundPrefix) || processedOutTags[outName] || !ok {
				finalRoutingRules = slices.Insert(finalRoutingRules, 0, rule)
				continue
			}
			setTag = func(rule []byte, outName string) ([]byte, error) {
				processedOutTags[outName] = true
				return sjson.SetBytes(rule, "outboundTag", outName)
			}
		}
		if balaTag.Exists() {
			regionSuffix = strings.TrimPrefix(balaTag.String(), autoSetupBalancerPrefix)
			_, ok := p.dnsRtAllRegionSuffix[regionSuffix]
			outName = balaTag.String()
			if !strings.HasPrefix(outName, autoSetupBalancerPrefix) || processedBalaTags[outName] || !ok {
				finalRoutingRules = slices.Insert(finalRoutingRules, 0, rule)
				continue
			}
			setTag = func(rule []byte, outName string) ([]byte, error) {
				processedBalaTags[outName] = true
				return sjson.SetBytes(rule, "balancerTag", outName)
			}
		}

		generatedRule := fmt.Sprintf(`      { // Auto-Generated conn-track rule for %s
        "type": "field",
        "source": "%s",
        "ip": "%s",
        "balancerTag": "",
        "outboundTag": ""
      }`, regionSuffix,
			routingRuleConnTrackSrcPrefix+outName, routingRuleConnTrackDstPrefix+outName)

		gRuleBytes, err := setTag(unsafe.Slice(unsafe.StringData(generatedRule), len(generatedRule)), outName)
		if err != nil {
			return fmt.Errorf("failed to set tag for generated routingeRule: %w", err)
		}
		addedCnt++
		finalRoutingRules = slices.Insert(finalRoutingRules, 0,
			rule, gjson.ParseBytes(gRuleBytes))
	}
	p.newRoutingRules = finalRoutingRules
	if addedCnt > 0 {
		slog.Info(fmt.Sprintf("Preparing new routingRules ... added %d auto-generated items", addedCnt))
	}
	return nil
}

func (p *Patcher) writeOutPatchedResult() (err error) {
	o := &sjson.Options{ReplaceInPlace: true}
	if len(p.newOutbounds) > 0 {
		p.Output, err = sjson.SetRawBytesOptions(p.Output, pathOutbounds, p.toRawBytes(p.newOutbounds), o)
		if err != nil {
			return
		}
	}
	if len(p.newBalancers) > 0 {
		p.Output, err = sjson.SetRawBytesOptions(p.Output, pathBalancers, p.toRawBytes(p.newBalancers), o)
		if err != nil {
			return
		}
	}
	if len(p.newObservers) > 0 {
		p.Output, err = sjson.SetRawBytesOptions(p.Output, pathObservers, p.toRawBytes(p.newObservers), o)
		if err != nil {
			return
		}
	}
	if len(p.newRoutingRules) > 0 {
		p.Output, err = sjson.SetRawBytesOptions(p.Output, pathRoutingRules, p.toRawBytes(p.newRoutingRules), o)
		if err != nil {
			return
		}
	}
	err = os.WriteFile(v2rayConfigPath, p.Output, 0644)
	if err != nil {
		slog.Error(fmt.Sprintf("Failed to write out patched file: %v", err))
	} else {
		slog.Info(fmt.Sprintf("Successfully wrote out patched file: %s", v2rayConfigPath))
	}
	return
}

func (p *Patcher) toRawBytes(rs []gjson.Result) []byte {
	buf := &bytes.Buffer{}
	buf.WriteByte('[')
	for idx, r := range rs {
		_, _ = buf.Write(unsafe.Slice(unsafe.StringData(r.Raw), len(r.Raw)))
		if idx < len(rs)-1 {
			buf.WriteByte(',')
		}
	}
	buf.WriteByte(']')
	return buf.Bytes()
}
