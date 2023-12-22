/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Code generated by lister-gen. DO NOT EDIT.

package v1beta2

import (
	v1beta2 "github.com/longhorn/longhorn-manager/k8s/pkg/apis/longhorn/v1beta2"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
)

// SupportBundleLister helps list SupportBundles.
type SupportBundleLister interface {
	// List lists all SupportBundles in the indexer.
	List(selector labels.Selector) (ret []*v1beta2.SupportBundle, err error)
	// SupportBundles returns an object that can list and get SupportBundles.
	SupportBundles(namespace string) SupportBundleNamespaceLister
	SupportBundleListerExpansion
}

// supportBundleLister implements the SupportBundleLister interface.
type supportBundleLister struct {
	indexer cache.Indexer
}

// NewSupportBundleLister returns a new SupportBundleLister.
func NewSupportBundleLister(indexer cache.Indexer) SupportBundleLister {
	return &supportBundleLister{indexer: indexer}
}

// List lists all SupportBundles in the indexer.
func (s *supportBundleLister) List(selector labels.Selector) (ret []*v1beta2.SupportBundle, err error) {
	err = cache.ListAll(s.indexer, selector, func(m interface{}) {
		ret = append(ret, m.(*v1beta2.SupportBundle))
	})
	return ret, err
}

// SupportBundles returns an object that can list and get SupportBundles.
func (s *supportBundleLister) SupportBundles(namespace string) SupportBundleNamespaceLister {
	return supportBundleNamespaceLister{indexer: s.indexer, namespace: namespace}
}

// SupportBundleNamespaceLister helps list and get SupportBundles.
type SupportBundleNamespaceLister interface {
	// List lists all SupportBundles in the indexer for a given namespace.
	List(selector labels.Selector) (ret []*v1beta2.SupportBundle, err error)
	// Get retrieves the SupportBundle from the indexer for a given namespace and name.
	Get(name string) (*v1beta2.SupportBundle, error)
	SupportBundleNamespaceListerExpansion
}

// supportBundleNamespaceLister implements the SupportBundleNamespaceLister
// interface.
type supportBundleNamespaceLister struct {
	indexer   cache.Indexer
	namespace string
}

// List lists all SupportBundles in the indexer for a given namespace.
func (s supportBundleNamespaceLister) List(selector labels.Selector) (ret []*v1beta2.SupportBundle, err error) {
	err = cache.ListAllByNamespace(s.indexer, s.namespace, selector, func(m interface{}) {
		ret = append(ret, m.(*v1beta2.SupportBundle))
	})
	return ret, err
}

// Get retrieves the SupportBundle from the indexer for a given namespace and name.
func (s supportBundleNamespaceLister) Get(name string) (*v1beta2.SupportBundle, error) {
	obj, exists, err := s.indexer.GetByKey(s.namespace + "/" + name)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, errors.NewNotFound(v1beta2.Resource("supportbundle"), name)
	}
	return obj.(*v1beta2.SupportBundle), nil
}
