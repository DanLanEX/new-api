package controller

import (
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/ionet"
)

func init() {
	ionet.SetProxyURLProvider(func() string {
		common.OptionMapRWMutex.RLock()
		proxyURL := strings.TrimSpace(common.OptionMap["model_deployment.ionet.proxy"])
		common.OptionMapRWMutex.RUnlock()
		return proxyURL
	})
}
