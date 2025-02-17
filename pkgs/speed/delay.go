package speed

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type DelayResult struct {
	duration time.Duration
	body     string
}

func (st *CFSpeedTest) GetDelayTestURL() string {
	var protocol string
	if st.EnableTLS {
		protocol = "https://"
	} else {
		protocol = "http://"
	}
	requestURL := fmt.Sprintf("%s%s/cdn-cgi/trace", protocol, st.DelayTestURL)
	return requestURL
}

func (st *CFSpeedTest) showPercentText(count *atomic.Int64, okCount *atomic.Int64, total *int) {
	percentage := float64(count.Load()) / float64(*total) * 100
	fmt.Printf("已完成: %d/%d(%.2f%%)，有效个数：%d", count.Load(), *total, percentage, okCount.Load())
	if count.Load() == int64(*total) {
		fmt.Printf("\n")
	} else {
		fmt.Printf("\r")
	}
}

func (st *CFSpeedTest) showPercent(stop <-chan struct{}, count *atomic.Int64, okCount *atomic.Int64, total *int) {
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				st.showPercentText(count, okCount, total)
			}
		}
	}()
}

func (st *CFSpeedTest) TestDelay(ips []IpPair) chan Result {
	var wg sync.WaitGroup

	resultChan := make(chan Result, len(ips))

	thread := make(chan struct{}, st.MaxThread)

	count := atomic.Int64{}
	okCount := atomic.Int64{}
	total := len(ips)
	stopShowPercent := make(chan struct{})
	st.showPercent(stopShowPercent, &count, &okCount, &total)
	for _, ip := range ips {
		// 如果满足延迟测试条数，则跳过
		if st.MaxDelayCount > 0 && okCount.Load() >= int64(st.MaxDelayCount) {
			break
		}

		wg.Add(1)
		thread <- struct{}{}
		go func(ipPair IpPair) {
			defer func() {
				wg.Done()
				count.Add(1)
				<-thread
			}()

			var result *Result
			var err error
			if st.DelayTestType == 1 {
				result, err = st.TestTCP(ipPair)
			} else if st.DelayTestType == 0 {
				result, err = st.TestDelayOnce(ipPair)
			} else {
				return
			}

			if result != nil {
				filterStr := ""
				if st.FilterIATASet != nil && st.FilterIATASet[result.dataCenter] == nil {
					filterStr = "，但被过滤"
				} else {
					resultChan <- *result
					okCount.Add(1)
				}
				fmt.Printf("发现有效IP %s 位置信息 %s 延迟 %d 毫秒%s\n", ipPair.String(),result.city, result.tcpDuration.Milliseconds(), filterStr)
			}
			if err != nil && st.VerboseMode {
				fmt.Printf("IP %s 错误, err: %s \n", ipPair.String(), err)
			}

		}(ip)
	}

	wg.Wait()
	stopShowPercent <- struct{}{}
	close(stopShowPercent)
	close(resultChan)
	st.showPercentText(&count, &okCount, &total)
	if st.MaxDelayCount > 0 && okCount.Load() >= int64(st.MaxDelayCount) {
		fmt.Printf("已满足最大延迟测试个数，跳过剩下延迟测试，符合个数：%d \n", okCount.Load())
	}
	return resultChan
}

func (st *CFSpeedTest) TestTCP(ipPair IpPair) (*Result, error) {
	dialer := &net.Dialer{
		Timeout:   timeout,
		KeepAlive: 0,
	}
	start := time.Now()
	conn, err := dialer.Dial("tcp", net.JoinHostPort(ipPair.ip, strconv.Itoa(ipPair.port)))
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	tcpDuration := time.Since(start)
	return &Result{ipPair.ip, ipPair.port, "", "", "", "", fmt.Sprintf("%d", tcpDuration.Milliseconds()), tcpDuration}, nil
}

func (st *CFSpeedTest) TestDelayOnce(ipPair IpPair) (*Result, error) {
	delayResult, err := st.TestDelayUseH1(ipPair)
	if err != nil {
		return nil, err
	}
	tcpDuration := delayResult.duration

	if strings.Contains(delayResult.body, "uag=Mozilla/5.0") {
		if matches := regexp.MustCompile(`colo=([A-Z]+)`).FindStringSubmatch(delayResult.body); len(matches) > 1 {
			if st.TestWebSocket {
				ok, err := st.TestWebSocketDelay(ipPair)
				if !ok {
					return nil, err
				}
			}

			dataCenter := matches[1]
			loc, ok := st.LocationMap[dataCenter]
			if ok {
				return &Result{ipPair.ip, ipPair.port, dataCenter, loc.Region, loc.Cca2, loc.City, fmt.Sprintf("%d", tcpDuration.Milliseconds()), tcpDuration}, nil
			} else {
				return &Result{ipPair.ip, ipPair.port, dataCenter, "", "", "", fmt.Sprintf("%d", tcpDuration.Milliseconds()), tcpDuration}, nil
			}
		}
	}
	return nil, fmt.Errorf("not match")
}

func (st *CFSpeedTest) TestWebSocketDelay(ipPair IpPair) (bool, error) {
	dialer := &net.Dialer{
		Timeout:   timeout,
		KeepAlive: 0,
	}
	conn, err := dialer.Dial("tcp", net.JoinHostPort(ipPair.ip, strconv.Itoa(ipPair.port)))
	if err != nil {
		if st.VerboseMode {
			fmt.Printf("connect failed, ip: %s err: %s\n", ipPair.String(), err)
		}
		return false, err
	}
	defer conn.Close()

	client := http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // 跳过证书验证
			Dial: func(network, addr string) (net.Conn, error) {
				return conn, nil
			},
		},
		Timeout: timeout,
	}

	var protocol string
	if st.EnableTLS {
		protocol = "https://"
	} else {
		protocol = "http://"
	}
	requestURL := fmt.Sprintf("%s%s/ws", protocol, st.DelayTestURL)

	req, _ := http.NewRequest("GET", requestURL, nil)

	// 添加用户代理
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "B5ReGbZ38Rrogrznmh1TFQ==")
	req.Close = true
	ctx, cancel := context.WithTimeout(context.Background(), maxDuration)
	defer cancel()
	resp, err := client.Do(req.WithContext(ctx))
	result := false
	if err == nil && resp != nil && resp.StatusCode == 101 {
		result = true
	}
	return result, fmt.Errorf("websocket: %s", err)
}
