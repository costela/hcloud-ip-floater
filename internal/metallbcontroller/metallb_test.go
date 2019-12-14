package metallbcontroller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"gopkg.in/yaml.v2"
)

const metalLBConfigWithoutOurPool = `
peers:
  - peer-address: 10.0.0.1
    peer-asn: 64501
    my-asn: 64500
address-pools:
  - name: something_with_bgp
    protocol: bgp
    addresses:
      - 198.51.100.0/24
    bgp-advertisements:
      - aggregation-length: 32
    localpref: 100
    communities:
      - no-advertise
      - aggregation-length: 24
bgp-communities:
  no-advertise: 65535:65282
`
const metalLBConfigWithOurPool = `
peers:
  - peer-address: 10.0.0.1
    peer-asn: 64501
    my-asn: 64500
address-pools:
  - name: something_with_bgp
    protocol: bgp
    addresses:
      - 198.51.100.0/24
    bgp-advertisements:
      - aggregation-length: 32
    localpref: 100
    communities:
      - no-advertise
      - aggregation-length: 24
  - name: hcloud-ip-floater
    protocol: layer2
    addresses:
      - 1.2.3.4/32
bgp-communities:
  no-advertise: 65535:65282
`

const expectedConfigWithOurIPs = `
peers:
  - peer-address: 10.0.0.1
    peer-asn: 64501
    my-asn: 64500
address-pools:
  - name: something_with_bgp
    protocol: bgp
    addresses:
      - 198.51.100.0/24
    bgp-advertisements:
      - aggregation-length: 32
    localpref: 100
    communities:
      - no-advertise
      - aggregation-length: 24
  - name: hcloud-ip-floater
    protocol: layer2
    addresses:
      - 1.1.1.1/32
      - 2.2.2.2/32
bgp-communities:
  no-advertise: 65535:65282
`
const expectedConfigWithoutOurIPs = `
peers:
  - peer-address: 10.0.0.1
    peer-asn: 64501
    my-asn: 64500
address-pools:
  - name: something_with_bgp
    protocol: bgp
    addresses:
      - 198.51.100.0/24
    bgp-advertisements:
      - aggregation-length: 32
    localpref: 100
    communities:
      - no-advertise
      - aggregation-length: 24
  - name: hcloud-ip-floater
    protocol: layer2
    addresses: []
bgp-communities:
  no-advertise: 65535:65282
`

func Test_mergeConfigs(t *testing.T) {
	type args struct {
		yamlSrc []byte
		ips     []string
	}
	tests := []struct {
		name    string
		args    args
		want    string
		wantErr bool
	}{
		{
			"config without our pool",
			args{[]byte(metalLBConfigWithoutOurPool), []string{"1.1.1.1/32", "2.2.2.2/32"}},
			expectedConfigWithOurIPs,
			false,
		},
		{
			"config with our pool",
			args{[]byte(metalLBConfigWithOurPool), []string{"1.1.1.1/32", "2.2.2.2/32"}},
			expectedConfigWithOurIPs,
			false,
		},
		{
			"error for nil addresses",
			args{[]byte(metalLBConfigWithoutOurPool), nil},
			expectedConfigWithoutOurIPs,
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := mergeConfigs(tt.args.yamlSrc, tt.args.ips)
			if (err != nil) != tt.wantErr {
				t.Errorf("mergeConfigs() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			want := metalLBConfig{}
			err = yaml.Unmarshal([]byte(tt.want), &want)
			assert.Nil(t, err)

			assert.EqualValues(t, &want, got)
		})
	}
}
