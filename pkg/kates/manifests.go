package kates

import (
	"bufio"
	"bytes"
	"io"
	"strings"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes/scheme"
	gw "sigs.k8s.io/gateway-api/apis/v1alpha1"
	"sigs.k8s.io/yaml"

	amb "github.com/datawire/ambassador/v2/pkg/api/getambassador.io/v3alpha1"
	"github.com/datawire/ambassador/v2/pkg/kates/k8sresourceparts"
	"github.com/datawire/ambassador/v2/pkg/kates/k8sresourcetypes"
)

var sch = runtime.NewScheme()

func init() {
	if err := scheme.AddToScheme(sch); err != nil {
		panic(err) // panic is ok in init() I guess
	}
	if err := apiextensions.AddToScheme(sch); err != nil {
		panic(err) // panic is ok in init() I guess
	}
	if err := amb.AddToScheme(sch); err != nil {
		panic(err) // panic is ok in init() I guess
	}
	if err := gw.AddToScheme(sch); err != nil {
		panic(err) // panic is ok in init() I guess
	}
}

func NewObject(kind, version string) (Object, error) {
	return newFromGVK(schema.FromAPIVersionAndKind(version, kind))
}

func newFromGVK(gvk schema.GroupVersionKind) (Object, error) {
	if sch.Recognizes(gvk) {
		robj, err := sch.New(gvk)
		if err != nil {
			return nil, err
		}
		return robj.(Object), nil
	} else {
		un := &k8sresourcetypes.Unstructured{}
		un.SetGroupVersionKind(gvk)
		return un, nil
	}
}

// NewObjectFromUnstructured will construct a new specialized object based on the runtime schema
// ambassador uses. This gaurantees any types defined by or used by ambassador will be constructed
// as the proper golang type.
func NewObjectFromUnstructured(unstructured *k8sresourcetypes.Unstructured) (Object, error) {
	if unstructured == nil {
		return nil, nil
	}

	gvk := unstructured.GetObjectKind().GroupVersionKind()

	obj, err := newFromGVK(gvk)
	if err != nil {
		return nil, err
	}
	err = convert(unstructured, &obj)
	if err != nil {
		return nil, err
	}

	return obj, nil
}

func NewUnstructured(kind, version string) *k8sresourcetypes.Unstructured {
	uns := &k8sresourcetypes.Unstructured{}
	uns.SetGroupVersionKind(schema.FromAPIVersionAndKind(version, kind))
	return uns
}

// Convert a potentially typed Object to an *k8sresourcetypes.Unstructured object.
func NewUnstructuredFromObject(obj Object) (result *k8sresourcetypes.Unstructured, err error) {
	err = convert(obj, &result)
	return
}

func parseManifests(text string, structured bool) ([]Object, error) {
	yr := utilyaml.NewYAMLReader(bufio.NewReader(strings.NewReader(text)))

	var result []Object

	for {
		bs, err := yr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		empty := true
		for _, line := range bytes.Split(bs, []byte("\n")) {
			if len(bytes.TrimSpace(bytes.SplitN(line, []byte("#"), 2)[0])) > 0 {
				empty = false
				break
			}
		}
		if empty {
			continue
		}

		var tm k8sresourceparts.TypeMeta
		err = yaml.Unmarshal(bs, &tm)
		if err != nil {
			return nil, err
		}
		var obj Object

		if structured {
			obj, err = newFromGVK(tm.GroupVersionKind())
			if err != nil {
				return nil, err
			}
		} else {
			un := &k8sresourcetypes.Unstructured{}
			un.SetGroupVersionKind(tm.GroupVersionKind())
			obj = un
		}

		err = yaml.Unmarshal(bs, obj)
		if err != nil {
			return nil, err
		}

		result = append(result, obj)
	}

	return result, nil

}

func ParseManifestsToUnstructured(text string) ([]Object, error) {
	return parseManifests(text, false)
}

func ParseManifests(text string) ([]Object, error) {
	return parseManifests(text, true)
}

func HasOwnerReference(owner, other Object) bool {
	refs := other.GetOwnerReferences()
	for _, r := range refs {
		if r.UID == owner.GetUID() {
			return true
		}
	}
	return false
}

func SetOwnerReferences(owner Object, objects ...Object) {
	gvk := owner.GetObjectKind().GroupVersionKind()
	for _, o := range objects {
		if !HasOwnerReference(owner, o) {
			ref := v1.NewControllerRef(owner, gvk)
			o.SetOwnerReferences(append(o.GetOwnerReferences(), *ref))
		}
	}
}

func MergeUpdate(target *k8sresourcetypes.Unstructured, source *k8sresourcetypes.Unstructured) {
	annotations := make(map[string]string)
	for k, v := range target.GetAnnotations() {
		annotations[k] = v
	}
	for k, v := range source.GetAnnotations() {
		annotations[k] = v
	}
	target.SetAnnotations(annotations)

	labels := make(map[string]string)
	for k, v := range target.GetLabels() {
		labels[k] = v
	}
	for k, v := range source.GetLabels() {
		labels[k] = v
	}
	target.SetLabels(labels)

	target.SetOwnerReferences(source.GetOwnerReferences())

	spec, ok := source.Object["spec"]
	if ok {
		target.Object["spec"] = spec
	} else {
		delete(target.Object, "spec")
	}
}
