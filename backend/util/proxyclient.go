package util

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	_ client.Client = &ProxyClient{}
)

// ProxyClient is a simple utility function/struct that maybe used to write unit tests that mock the K8s go client.
// ProxyClient wraps an existing client.Client, and calls a listener based on the result of the call.
// This allows the test to verify custom conditions, or that particular calls were made.
// (Yes there are other Go modules that do this, as well).
type ProxyClient struct {
	InnerClient client.Client
	Informer    ProxyClientEventReceiver
}

type ProxyClientEvent struct {
	Action   ClientAction
	Ctx      context.Context
	Key      *client.ObjectKey
	Obj      *client.Object
	Patch    *client.Patch
	List     *client.ObjectList
	Options  *ProxyClientEventOptions
	ErrorRes error
	ExitTime time.Time
}

type ProxyClientEventOptions struct {
	List        []client.ListOption
	Create      []client.CreateOption
	Delete      []client.DeleteOption
	Update      []client.UpdateOption
	Patch       []client.PatchOption
	DeleteAllOf []client.DeleteAllOfOption
}

type ClientAction string

const (
	Get          ClientAction = "Get"
	List         ClientAction = "List"
	Create       ClientAction = "Create"
	Delete       ClientAction = "Delete"
	Update       ClientAction = "Update"
	Patch        ClientAction = "Patch"
	DeleteAllOf  ClientAction = "DeleteAllOf"
	Status       ClientAction = "Status"
	StatusUpdate ClientAction = "StatusUpdate"
	StatusPatch  ClientAction = "StatusPatch"
	Scheme       ClientAction = "Scheme"
	RESTMapper   ClientAction = "RESTMapper"
)

// Get retrieves an obj for the given object key from the Kubernetes Cluster.
// obj must be a struct pointer so that obj can be updated with the response
// returned by the Server.
func (pc *ProxyClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	res := pc.InnerClient.Get(ctx, key, obj, opts...)

	if pc.Informer != nil {
		event := ProxyClientEvent{
			Action:   Get,
			Ctx:      ctx,
			Key:      &key,
			Obj:      &obj,
			ErrorRes: res,
			ExitTime: time.Now(),
		}
		pc.Informer.ReceiveEvent(event)
	}

	return res
}

// List retrieves list of objects for a given namespace and list options. On a
// successful call, Items field in the list will be populated with the
// result returned from the server.
func (pc *ProxyClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	res := pc.InnerClient.List(ctx, list, opts...)

	if pc.Informer != nil {
		event := ProxyClientEvent{
			Action: List,
			Ctx:    ctx,
			List:   &list,
			Options: &ProxyClientEventOptions{
				List: opts,
			},
			ErrorRes: res,
			ExitTime: time.Now(),
		}
		pc.Informer.ReceiveEvent(event)
	}

	return res
}

// Create saves the object obj in the Kubernetes cluster.
func (pc *ProxyClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	res := pc.InnerClient.Create(ctx, obj, opts...)

	if pc.Informer != nil {
		event := ProxyClientEvent{
			Action: Create,
			Ctx:    ctx,
			Obj:    &obj,
			Options: &ProxyClientEventOptions{
				Create: opts,
			},
			ErrorRes: res,
			ExitTime: time.Now(),
		}
		pc.Informer.ReceiveEvent(event)
	}

	return res
}

// Delete deletes the given obj from Kubernetes cluster.
func (pc *ProxyClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	res := pc.InnerClient.Delete(ctx, obj, opts...)

	if pc.Informer != nil {
		event := ProxyClientEvent{
			Action: Delete,
			Ctx:    ctx,
			Obj:    &obj,
			Options: &ProxyClientEventOptions{
				Delete: opts,
			},
			ErrorRes: res,
			ExitTime: time.Now(),
		}
		pc.Informer.ReceiveEvent(event)
	}

	return res
}

// Update updates the given obj in the Kubernetes cluster. obj must be a
// struct pointer so that obj can be updated with the content returned by the Server.
func (pc *ProxyClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	res := pc.InnerClient.Update(ctx, obj, opts...)

	if pc.Informer != nil {
		event := ProxyClientEvent{
			Action: Update,
			Ctx:    ctx,
			Obj:    &obj,
			Options: &ProxyClientEventOptions{
				Update: opts,
			},
			ErrorRes: res,
			ExitTime: time.Now(),
		}
		pc.Informer.ReceiveEvent(event)
	}

	return res
}

// Patch patches the given obj in the Kubernetes cluster. obj must be a
// struct pointer so that obj can be updated with the content returned by the Server.
func (pc *ProxyClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	res := pc.InnerClient.Patch(ctx, obj, patch, opts...)

	if pc.Informer != nil {
		event := ProxyClientEvent{
			Action: Patch,
			Ctx:    ctx,
			Obj:    &obj,
			Patch:  &patch,
			Options: &ProxyClientEventOptions{
				Patch: opts,
			},
			ErrorRes: res,
			ExitTime: time.Now(),
		}
		pc.Informer.ReceiveEvent(event)
	}

	return res
}

// DeleteAllOf deletes all objects of the given type matching the given options.
func (pc *ProxyClient) DeleteAllOf(ctx context.Context, obj client.Object, opts ...client.DeleteAllOfOption) error {
	res := pc.InnerClient.DeleteAllOf(ctx, obj, opts...)

	if pc.Informer != nil {
		event := ProxyClientEvent{
			Action: DeleteAllOf,
			Ctx:    ctx,
			Obj:    &obj,
			Options: &ProxyClientEventOptions{
				DeleteAllOf: opts,
			},
			ErrorRes: res,
			ExitTime: time.Now(),
		}
		pc.Informer.ReceiveEvent(event)
	}

	return res
}

// StatusWriter knows how to update status subresource of a Kubernetes object.
func (pc *ProxyClient) Status() client.StatusWriter {
	res := pc.InnerClient.Status()
	return res
}

// Scheme returns the scheme this client is using.
func (pc *ProxyClient) Scheme() *runtime.Scheme {
	res := pc.InnerClient.Scheme()
	return res

}

// RESTMapper returns the rest this client is using.
func (pc *ProxyClient) RESTMapper() meta.RESTMapper {
	res := pc.InnerClient.RESTMapper()
	return res
}

type ProxyClientEventReceiver interface {
	ReceiveEvent(event ProxyClientEvent)
}

type ProxyClientStatusWrapper struct {
	innerWriter *client.StatusWriter
	parent      *ProxyClient
}

// Update updates the fields corresponding to the status subresource for the
// given obj. obj must be a struct pointer so that obj can be updated
// with the content returned by the Server.
func (pcsw *ProxyClientStatusWrapper) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {

	res := (*pcsw.innerWriter).Update(ctx, obj, opts...)

	if pcsw.parent.Informer != nil {
		event := ProxyClientEvent{
			Action: StatusUpdate,
			Ctx:    ctx,
			Obj:    &obj,
			Options: &ProxyClientEventOptions{
				Update: opts,
			},
			ErrorRes: res,
		}
		pcsw.parent.Informer.ReceiveEvent(event)
	}

	return res

}

// Patch patches the given object's subresource. obj must be a struct
// pointer so that obj can be updated with the content returned by the
// Server.
func (pcsw *ProxyClientStatusWrapper) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {

	res := (*pcsw.innerWriter).Patch(ctx, obj, patch, opts...)

	if pcsw.parent.Informer != nil {
		event := ProxyClientEvent{
			Action: StatusPatch,
			Ctx:    ctx,
			Obj:    &obj,
			Patch:  &patch,
			Options: &ProxyClientEventOptions{
				Patch: opts,
			},
			ErrorRes: res,
		}
		pcsw.parent.Informer.ReceiveEvent(event)
	}

	return res
}

// String returns a simple string representation of the event
func (event ProxyClientEvent) String() string {

	time := fmt.Sprintf("%02d:%02d:%02d.%03d",
		event.ExitTime.Hour(), event.ExitTime.Minute(), event.ExitTime.Second(), event.ExitTime.Nanosecond()/1000000)

	res := fmt.Sprintf("[%v] %s ", time, event.Action)

	if event.Obj != nil {

		objJson, err := json.Marshal(*event.Obj)
		if err != nil {
			return err.Error()
		}

		res += fmt.Sprintf("%v %v ", event.ObjectTypeOf(), string(objJson))
	}

	if event.ErrorRes != nil {
		res += "err: " + event.ErrorRes.Error() + " "
	}

	return strings.TrimSpace(res)

}

func (event ProxyClientEvent) ObjectTypeOf() string {
	if event.Obj == nil {
		return "(nil)"
	}

	res := reflect.TypeOf(*event.Obj).String()

	res = res[strings.Index(res, ".")+1:]

	return res
}

// ListEventReceiver is a simple event receiver implementation that logs all events to a slice.
type ListEventReceiver struct {
	Events []ProxyClientEvent
}

func (li *ListEventReceiver) ReceiveEvent(event ProxyClientEvent) {
	li.Events = append(li.Events, event)
}
