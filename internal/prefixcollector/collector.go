// Copyright (c) 2020 Doc.ai and/or its affiliates.
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at:
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package prefixcollector contains excluded prefix collector, prefix sources and functions working with them
package prefixcollector

import (
	"cmd-exclude-prefixes-k8s/internal/utils"
	"context"
	"sync"

	"github.com/networkservicemesh/sdk/pkg/tools/log"

	apiV1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/watch"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/ghodss/yaml"

	"github.com/sirupsen/logrus"

	"github.com/networkservicemesh/sdk/pkg/tools/prefixpool"
)

const (
	configMapKey = "excluded_prefixes.yaml"
)

// ExcludePrefixSource is source of excluded prefixes
type ExcludePrefixSource interface {
	Prefixes() []string
}

// Notifier is entity used for listeners notification
type Notifier interface {
	Broadcast()
}

// ExcludePrefixCollector is service, collecting excluded prefixes from list of ExcludePrefixSource
// and writing result to outputFilePath in yaml format
type ExcludePrefixCollector struct {
	notify                *sync.Cond
	nsmConfigMapName      string
	nsmConfigMapNamespace string
	sources               []ExcludePrefixSource
	previousPrefixes      *utils.SynchronizedPrefixesContainer
	configMapInterface    v1.ConfigMapInterface
}

// NewExcludePrefixCollector creates ExcludePrefixCollector
func NewExcludePrefixCollector(notify *sync.Cond, configMapName, configMapNamespace string,
	sources ...ExcludePrefixSource) *ExcludePrefixCollector {
	return &ExcludePrefixCollector{
		nsmConfigMapName:      configMapName,
		nsmConfigMapNamespace: configMapNamespace,
		notify:                notify,
		sources:               sources,
		previousPrefixes:      utils.NewSynchronizedPrefixesContainer(),
	}
}

// Serve - begin monitoring sources.
// Updates exclude prefix file after every notification.
func (epc *ExcludePrefixCollector) Serve(ctx context.Context) {
	epc.configMapInterface = KubernetesInterface(ctx).CoreV1().ConfigMaps(epc.nsmConfigMapNamespace)
	go func() {
		epc.monitorNSMConfigMap(ctx)
		epc.notify.Broadcast()
	}()

	// check current state of sources
	epc.updateExcludedPrefixesConfigmap(ctx)

	for ctx.Err() == nil {
		epc.notify.L.Lock()
		epc.notify.Wait()
		epc.notify.L.Unlock()
		epc.updateExcludedPrefixesConfigmap(ctx)
	}
}

func (epc *ExcludePrefixCollector) monitorNSMConfigMap(ctx context.Context) {
	watcher, err := epc.configMapInterface.Watch(ctx, metav1.ListOptions{})
	if err != nil {
		logrus.Fatalf("Error watching config map: %v", err)
	}

	logEntry := log.Entry(ctx).WithFields(logrus.Fields{
		"configmap-namespace": epc.nsmConfigMapNamespace,
		"configmap-name":      epc.nsmConfigMapName,
	})

	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return
			}

			configMap, ok := event.Object.(*apiV1.ConfigMap)
			if !ok || configMap.Name != epc.nsmConfigMapName {
				continue
			}

			switch event.Type {
			case watch.Error:
				logEntry.Errorf("Error during nsm configmap watch: %v", err)
				return
			case watch.Modified:
				prefixes, err := utils.YamlToPrefixes([]byte(configMap.Data[configMapKey]))
				if err != nil || !utils.UnorderedSlicesEquals(prefixes, epc.previousPrefixes.Load()) {
					logEntry.Warn("Nsm configmap excluded prefixes field external change, restoring last state")
					epc.updateConfigMap(ctx, epc.previousPrefixes.Load(), configMap)
				}
			}
		}
	}
}

func (epc *ExcludePrefixCollector) updateExcludedPrefixesConfigmap(ctx context.Context) {
	excludePrefixPool, _ := prefixpool.New()

	for _, v := range epc.sources {
		sourcePrefixes := v.Prefixes()
		if len(sourcePrefixes) == 0 {
			continue
		}

		if err := excludePrefixPool.ReleaseExcludedPrefixes(v.Prefixes()); err != nil {
			logrus.Error(err)
			return
		}
	}

	newPrefixes := excludePrefixPool.GetPrefixes()
	if utils.UnorderedSlicesEquals(newPrefixes, epc.previousPrefixes.Load()) {
		return
	}

	configMap, err := epc.configMapInterface.Get(ctx, epc.nsmConfigMapName, metav1.GetOptions{})
	if err != nil {
		logrus.Fatalf("Failed to get NSM ConfigMap '%s/%s': %v", epc.nsmConfigMapNamespace, epc.nsmConfigMapName, err)
		return
	}

	epc.previousPrefixes.Store(newPrefixes)
	epc.updateConfigMap(ctx, newPrefixes, configMap)
}

func (epc *ExcludePrefixCollector) updateConfigMap(ctx context.Context, newPrefixes []string, configMap *apiV1.ConfigMap) {
	data, err := prefixesToYaml(newPrefixes)
	if err != nil {
		logrus.Errorf("Can not create marshal prefixes, err: %v", err.Error())
		return
	}

	configMap.Data[configMapKey] = string(data)

	_, err = epc.configMapInterface.Update(ctx, configMap, metav1.UpdateOptions{})
	if err != nil {
		logrus.Errorf("Failed to update NSM ConfigMap '%s/%s': %v", epc.nsmConfigMapNamespace, epc.nsmConfigMapName, err)
		return
	}
	logrus.Infof("Excluded prefixes were successfully updated: %v", newPrefixes)
}

// prefixesToYaml converts list of prefixes to yaml file
func prefixesToYaml(prefixesList []string) ([]byte, error) {
	source := struct {
		Prefixes []string
	}{prefixesList}

	bytes, err := yaml.Marshal(source)
	if err != nil {
		return nil, err
	}

	return bytes, nil
}
