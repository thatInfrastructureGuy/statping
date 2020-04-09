// Statping
// Copyright (C) 2018.  Hunter Long and the project contributors
// Written by Hunter Long <info@socialeck.com> and the project contributors
//
// https://github.com/hunterlong/statping
//
// The licenses for most software and other practical works are designed
// to take away your freedom to share and change the works.  By contrast,
// the GNU General Public License is intended to guarantee your freedom to
// share and change all versions of a program--to make sure it remains free
// software for all its users.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package core

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hunterlong/statping/core/notifier"
	"github.com/hunterlong/statping/types"
	"github.com/hunterlong/statping/utils"
	"github.com/tatsushid/go-fastping"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// checkServices will start the checking go routine for each service
func checkServices() {
	log.Infoln(fmt.Sprintf("Starting monitoring process for %v Services", len(CoreApp.Services)))
	for _, ser := range CoreApp.Services {
		//go obj.StartCheckins()
		go ser.CheckQueue(true)
	}
}

// Check will run checkHttp for HTTP services and checkTcp for TCP services
// if record param is set to true, it will add a record into the database.
func (s *Service) Check(record bool) {
	switch s.Type {
	case "http":
		s.checkHttp(record)
	case "grpc":
		s.checkGrpc(record)
	case "tcp", "udp":
		s.checkTcp(record)
	case "icmp":
		s.checkIcmp(record)
	}
}

// CheckQueue is the main go routine for checking a service
func (s *Service) CheckQueue(record bool) {
	s.Checkpoint = time.Now()
	s.SleepDuration = time.Duration((time.Duration(s.Id) * 100) * time.Millisecond)
CheckLoop:
	for {
		select {
		case <-s.Running:
			log.Infoln(fmt.Sprintf("Stopping service: %v", s.Name))
			break CheckLoop
		case <-time.After(s.SleepDuration):
			s.Check(record)
			s.Checkpoint = s.Checkpoint.Add(s.duration())
			sleep := s.Checkpoint.Sub(time.Now())
			if !s.Online {
				s.SleepDuration = s.duration()
			} else {
				s.SleepDuration = sleep
			}
		}
		continue
	}
}

// duration returns the amount of duration for a service to check its status
func (s *Service) duration() time.Duration {
	var amount time.Duration
	if s.Interval >= 10000 {
		amount = time.Duration(s.Interval) * time.Microsecond
	} else {
		amount = time.Duration(s.Interval) * time.Second
	}
	return amount
}

func (s *Service) parseHost() string {
	if s.Type == "tcp" || s.Type == "udp" || s.Type == "grpc" {
		return s.Domain
	} else {
		u, err := url.Parse(s.Domain)
		if err != nil {
			return s.Domain
		}
		return strings.Split(u.Host, ":")[0]
	}
}

// dnsCheck will check the domain name and return a float64 for the amount of time the DNS check took
func (s *Service) dnsCheck() (float64, error) {
	var err error
	t1 := time.Now()
	host := s.parseHost()
	if s.Type == "tcp" {
		_, err = net.LookupHost(host)
	} else {
		_, err = net.LookupIP(host)
	}
	if err != nil {
		return 0, err
	}
	t2 := time.Now()
	subTime := t2.Sub(t1).Seconds()
	return subTime, err
}

func isIPv6(address string) bool {
	return strings.Count(address, ":") >= 2
}

// checkIcmp will send a ICMP ping packet to the service
func (s *Service) checkIcmp(record bool) *Service {
	p := fastping.NewPinger()
	resolveIP := "ip4:icmp"
	if isIPv6(s.Domain) {
		resolveIP = "ip6:icmp"
	}
	ra, err := net.ResolveIPAddr(resolveIP, s.Domain)
	if err != nil {
		recordFailure(s, fmt.Sprintf("Could not send ICMP to service %v, %v", s.Domain, err))
		return s
	}
	p.AddIPAddr(ra)
	p.OnRecv = func(addr *net.IPAddr, rtt time.Duration) {
		s.Latency = rtt.Seconds()
		recordSuccess(s)
	}
	err = p.Run()
	if err != nil {
		recordFailure(s, fmt.Sprintf("Issue running ICMP to service %v, %v", s.Domain, err))
		return s
	}
	s.LastResponse = ""
	return s
}

// checkTcp will check a TCP service
func (s *Service) checkTcp(record bool) *Service {
	dnsLookup, err := s.dnsCheck()
	if err != nil {
		if record {
			recordFailure(s, fmt.Sprintf("Could not get IP address for TCP service %v, %v", s.Domain, err))
		}
		return s
	}
	s.PingTime = dnsLookup
	t1 := time.Now()
	domain := fmt.Sprintf("%v", s.Domain)
	if s.Port != 0 {
		domain = fmt.Sprintf("%v:%v", s.Domain, s.Port)
		if isIPv6(s.Domain) {
			domain = fmt.Sprintf("[%v]:%v", s.Domain, s.Port)
		}
	}
	conn, err := net.DialTimeout(s.Type, domain, time.Duration(s.Timeout)*time.Second)
	if err != nil {
		if record {
			recordFailure(s, fmt.Sprintf("Dial Error %v", err))
		}
		return s
	}
	if err := conn.Close(); err != nil {
		if record {
			recordFailure(s, fmt.Sprintf("%v Socket Close Error %v", strings.ToUpper(s.Type), err))
		}
		return s
	}
	t2 := time.Now()
	s.Latency = t2.Sub(t1).Seconds()
	s.LastResponse = ""
	if record {
		recordSuccess(s)
	}
	return s
}

// checkGrpc will check a GRPC service
func (s *Service) checkGrpc(record bool) *Service {
	var err error
	// Strip URL scheme if present. Eg: https:// , http://
	if strings.Contains(s.Domain, "://") {
		u, err := url.Parse(s.Domain)
		if err != nil {
			// Unable to parse.
			log.Warnln(fmt.Sprintf("GRPC Service: '%s', Unable to parse URL: '%v'", s.Name, s.Domain))
		}

		// Set domain as hostname without port number.
		s.Domain = u.Hostname()
	}

	// Calculate DNS check time
	s.PingTime, err = s.dnsCheck()
	if err != nil {
		if record {
			recordFailure(s, fmt.Sprintf("Could not get IP address for domain %v, %v", s.Domain, err))
		}
		return s
	}

	// Connect to grpc service without TLS certs.
	grpcOption := grpc.WithInsecure()

	// Check if TLS is enabled
	// Upgrade GRPC connection if using TLS
	// Force to connect on HTTP2 with TLS. Needed when using a reverse proxy such as nginx.
	if s.VerifySSL.Bool {
		h2creds := credentials.NewTLS(&tls.Config{NextProtos: []string{"h2"}})
		grpcOption = grpc.WithTransportCredentials(h2creds)
	}

	t1 := time.Now()
	// Set up a connection to the server.
	conn, err := grpc.Dial(s.Domain+":"+strconv.Itoa(s.Port), grpcOption)
	if err != nil {
		if record {
			recordFailure(s, fmt.Sprintf("GRPC Error %v", err))
		}
		return s
	}
	defer conn.Close()

	// Context will cancel the request when timeout is exceeded.
	// Cancel the context when request is served within the timeout limit.
	timeout := time.Duration(s.Timeout) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Create a new health check client
	c := healthpb.NewHealthClient(conn)
	in := &healthpb.HealthCheckRequest{}
	res, err := c.Check(ctx, in)
	if err != nil {
		if record {
			recordFailure(s, fmt.Sprintf("GRPC Error %v", err))
		}
		return s
	}

	// Record latency and response
	t2 := time.Now()
	s.Latency = t2.Sub(t1).Seconds()
	s.LastResponse = res.String()
	s.LastStatusCode = int(res.GetStatus())
	s.ExpectedStatus = 1
	s.Expected.String = "status:SERVING"

	if !(s.Expected.String == strings.TrimSpace(s.LastResponse)) {
		log.Warnln(fmt.Sprintf("GRPC Service: '%s', Response: expected '%v', got '%v'", s.Name, s.LastResponse, s.Expected.String))
		if record {
			recordFailure(s, fmt.Sprintf("GRPC Response Body did not match '%v'", s.Expected.String))
		}
		return s
	}

	if s.ExpectedStatus != int(res.Status) {
		if record {
			recordFailure(s, fmt.Sprintf("GRPC Service: '%s', Status Code: expected '%v', got '%v'", s.Name, res.Status, healthpb.HealthCheckResponse_ServingStatus(s.ExpectedStatus)))
		}
		return s
	}

	if record {
		recordSuccess(s)
	}

	return s
}

// checkHttp will check a HTTP service
func (s *Service) checkHttp(record bool) *Service {
	dnsLookup, err := s.dnsCheck()
	if err != nil {
		if record {
			recordFailure(s, fmt.Sprintf("Could not get IP address for domain %v, %v", s.Domain, err))
		}
		return s
	}
	s.PingTime = dnsLookup
	t1 := time.Now()

	timeout := time.Duration(s.Timeout) * time.Second
	var content []byte
	var res *http.Response

	var headers []string
	if s.Headers.Valid {
		headers = strings.Split(s.Headers.String, ",")
	} else {
		headers = nil
	}

	if s.Method == "POST" {
		content, res, err = utils.HttpRequest(s.Domain, s.Method, "application/json", headers, bytes.NewBuffer([]byte(s.PostData.String)), timeout, s.VerifySSL.Bool)
	} else {
		content, res, err = utils.HttpRequest(s.Domain, s.Method, nil, headers, nil, timeout, s.VerifySSL.Bool)
	}
	if err != nil {
		if record {
			recordFailure(s, fmt.Sprintf("HTTP Error %v", err))
		}
		return s
	}
	t2 := time.Now()
	s.Latency = t2.Sub(t1).Seconds()
	s.LastResponse = string(content)
	s.LastStatusCode = res.StatusCode

	if s.Expected.String != "" {
		match, err := regexp.MatchString(s.Expected.String, string(content))
		if err != nil {
			log.Warnln(fmt.Sprintf("Service %v expected: %v to match %v", s.Name, string(content), s.Expected.String))
		}
		if !match {
			if record {
				recordFailure(s, fmt.Sprintf("HTTP Response Body did not match '%v'", s.Expected))
			}
			return s
		}
	}
	if s.ExpectedStatus != res.StatusCode {
		if record {
			recordFailure(s, fmt.Sprintf("HTTP Status Code %v did not match %v", res.StatusCode, s.ExpectedStatus))
		}
		return s
	}
	if record {
		recordSuccess(s)
	}
	return s
}

// recordSuccess will create a new 'hit' record in the database for a successful/online service
func recordSuccess(s *Service) {
	s.UpdateNotify = CoreApp.UpdateNotify.Bool
	s.LastOnline = utils.Timezoner(time.Now().UTC(), CoreApp.Timezone)
	hit := &types.Hit{
		Service:   s.Id,
		Latency:   s.Latency,
		PingTime:  s.PingTime,
		CreatedAt: time.Now().UTC(),
	}
	s.CreateHit(hit)
	log.WithFields(utils.ToFields(hit, s.Select())).Infoln(fmt.Sprintf("Service %v Successful Response: %0.2f ms | Lookup in: %0.2f ms", s.Name, hit.Latency*1000, hit.PingTime*1000))
	notifier.OnSuccess(s.Service)
	s.Online = true
	s.SuccessNotified = true
	s.CurrentFailureCount = 0
}

// recordFailure will create a new 'Failure' record in the database for a offline service
func recordFailure(s *Service, issue string) {
	s.UpdateNotify = CoreApp.UpdateNotify.Bool
	fail := &types.Failure{
		Service:   s.Id,
		Issue:     issue,
		PingTime:  s.PingTime,
		CreatedAt: time.Now().UTC(),
		ErrorCode: s.LastStatusCode,
	}
	log.WithFields(utils.ToFields(fail, s.Select())).
		Warnln(fmt.Sprintf("Service %v Failing: %v | Lookup in: %0.2f ms", s.Name, issue, fail.PingTime*1000))
	s.CreateFailure(fail)
	s.Online = false
	s.SuccessNotified = false
	s.UpdateNotify = CoreApp.UpdateNotify.Bool
	s.DownText = s.DowntimeText()
	s.CurrentFailureCount++
	notifier.OnFailure(s.Service, fail)
}
