# V2Ray 订阅自动更新工具


```shell
Usage of ./patcher:
  -sub string
    	url address of v2ray subscription
  -v2ray-config string
    	v2ray jsonV4 config path (default "/usr/local/etc/v2ray/config.json")

```

## 功能
1. 自动根据给定URL更新`v2ray.json`
    * 目前仅支持`Vmess`协议订阅解析
2. 自动根据`dnsCircuit`配置，从指定订阅数据中匹配出对应的`Vmess`服务端，并自动补充至`outbounds`。
3. 若使用了负载均衡，则自动根据`dnsCircuit`配置，生成对应的`routing.balancers`和`multiObservatory.observers`配置。
4. 自动根据`dnsCircuit`配置，为`routing.rules`补充`dnsRoute`的`conn-track`规则。

## 使用要求
* 自动管理和更新的`outbound`Tag必须以`proxy-`开头。
* 自动管理的`dnsCircuit.balancerTags`必须以`balancer-proxy-`开头。
* 自动管理的`dnsCircuit.outboundTags`必须以`proxy-`开头。
* 自动管理的`multiObservatory.observers`必须以`observatory-internet-proxy-`开头。

## 原理
1. 使用 `strings.TrimSuffix({dnsCircuit.balancerTags}, "balancer-proxy-")` 和
`strings.TrimSuffix({dnsCircuit.outboundTags}, "proxy-")` 得到的字串做为`regionKey`。
2. 将订阅拿到的`Vmess`服务器域名，减去TLD+1剩下的部分，与上面的`regionKey`进行比对。
3. 若该服务器域名含有`regionKey`，则认为该服务器符合要求，会被自动补充至`outbounds`中。
4. `observatory` 和 `balancers` 通过类似方法自动生成并补充对应配置。
5. 从原有配置文件中清理掉所有符合上述"使用要求"前缀的项目，包括`outbounds`,`observatory`,`balancers`,`routing.rules`等。
6. 将剩下的配置和补充配置进行合并操作。
7. 最终得到订阅更新后的配置文件。

## 优点
* 只需要维护分流规则，订阅更新全自动。可通过cron等设置定期自动更新
* 增量式Patch操作，任何自定义配置均会原样保留。

## 待填坑
* 支持完整的`Vmess`订阅选项，目前仅支持地址/端口/UUID等。
* 支持识别其他协议类型`outbounds`订阅。
* 支持配置`balancer.fallbackTag`，目前会继承之前同名balancer的配置，或者使用最后一个不为空的自动管理的balancer配置。
* 支持其他类型的`observatory`配置，目前默认为`leastping`。