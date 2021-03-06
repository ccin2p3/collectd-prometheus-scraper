// +build go1.5,cgo

/*
Package plugin exports the functions required to write collectd plugins in Go.

This package provides the abstraction necessary to write plugins for collectd
in Go, compile them into a shared object and let the daemon load and use them.

Example plugin

To understand how this module is being used, please consider the following
example:

  package main

  import (
	  "time"

	  "collectd.org/api"
	  "collectd.org/plugin"
  )

  type ExamplePlugin struct{}

  func (*ExamplePlugin) Read() error {
	  vl := &api.ValueList{
		  Identifier: api.Identifier{
			  Host:   "example.com",
			  Plugin: "goplug",
			  Type:   "gauge",
		  },
		  Time:     time.Now(),
		  Interval: 10 * time.Second,
		  Values:   []api.Value{api.Gauge(42)},
		  DSNames:  []string{"value"},
	  }
	  if err := plugin.Write(context.Background(), vl); err != nil {
		  return err
	  }

	  return nil
  }

  func init() {
	  plugin.RegisterRead("example", &ExamplePlugin{})
  }

  func main() {} // ignored

The first step when writing a new plugin with this package, is to create a new
"main" package. Even though it has to have a main() function to make cgo happy,
the main() function is ignored. Instead, put your startup code into the init()
function which essentially takes on the same role as the module_register()
function in C based plugins.

Then, define a type which implements the Reader interface by implementing the
"Read() error" function. In the example above, this type is called
ExamplePlugin. Create an instance of this type and pass it to RegisterRead() in
the init() function.

Build flags

To compile your plugin, set up the CGO_CPPFLAGS environment variable and call
"go build" with the following options:

  export COLLECTD_SRC="/path/to/collectd"
  export CGO_CPPFLAGS="-I${COLLECTD_SRC}/src/daemon -I${COLLECTD_SRC}/src"
  go build -buildmode=c-shared -o example.so
*/
package plugin // import "collectd.org/plugin"

// #cgo CPPFLAGS: -DHAVE_CONFIG_H
// #cgo LDFLAGS: -ldl
// #include <stdlib.h>
// #include <dlfcn.h>
// #include "plugin.h"
//
// int dispatch_values_wrapper (value_list_t const *vl);
// int register_read_wrapper (char const *group, char const *name,
//     plugin_read_cb callback,
//     cdtime_t interval,
//     user_data_t *ud);
//
// data_source_t *ds_dsrc(data_set_t const *ds, size_t i);
//
// void value_list_add_counter (value_list_t *, counter_t);
// void value_list_add_derive  (value_list_t *, derive_t);
// void value_list_add_gauge   (value_list_t *, gauge_t);
// counter_t value_list_get_counter (value_list_t *, size_t);
// derive_t  value_list_get_derive  (value_list_t *, size_t);
// gauge_t   value_list_get_gauge   (value_list_t *, size_t);
//
// int wrap_read_callback(user_data_t *);
//
// int register_write_wrapper (char const *, plugin_write_cb, user_data_t *);
// int wrap_write_callback(data_set_t *, value_list_t *, user_data_t *);
//
// int register_shutdown_wrapper (char *, plugin_shutdown_cb);
// int wrap_shutdown_callback(void);
//
// meta_data_t *meta_data_create_wrapper(void);
// meta_data_t *meta_data_destroy_wrapper(meta_data_t *);
//
// int meta_data_add_string_wrapper(meta_data_t *md,
//   const char *key, const char *value);
// int meta_data_add_signed_int_wrapper(meta_data_t *md,
//   const char *key, int64_t value);
// int meta_data_add_unsigned_int_wrapper(meta_data_t *md,
//   const char *key, uint64_t value);
// int meta_data_add_double_wrapper(meta_data_t *md,
//   const char *key, double value);
// int meta_data_add_boolean_wrapper(meta_data_t *md,
//   const char *key, _Bool value);
import "C"

import (
	"context"
	"fmt"
	"time"
	"unsafe"

	"collectd.org/api"
	"collectd.org/cdtime"
)

const (
	defaultReadCallbackGroup = "golang"
)

var (
	ctx = context.Background()
)

// Reader defines the interface for read callbacks, i.e. Go functions that are
// called periodically from the collectd daemon.
type Reader interface {
	Read() error
}

func strcpy(dst []C.char, src string) {
	byteStr := []byte(src)
	cStr := make([]C.char, len(byteStr)+1)

	for i, b := range byteStr {
		cStr[i] = C.char(b)
	}
	cStr[len(cStr)-1] = C.char(0)

	copy(dst, cStr)
}

func newMetaDataT(meta api.Metadata) (*C.meta_data_t, error) {
	ret := C.meta_data_create_wrapper()

	for key, value := range meta {
		cKey := C.CString(key)

		switch v := value.(type) {
		case int64:
			C.meta_data_add_signed_int_wrapper(ret, cKey, C.long(v))
		case uint64:
			C.meta_data_add_unsigned_int_wrapper(ret, cKey, C.ulong(v))
		case float64:
			C.meta_data_add_double_wrapper(ret, cKey, C.double(v))
		case string:
			C.meta_data_add_string_wrapper(ret, cKey, C.CString(v))
		case bool:
			C.meta_data_add_boolean_wrapper(ret, cKey, C._Bool(v))
		default:
			C.meta_data_destroy_wrapper(ret)
			return nil, fmt.Errorf("not yet supported: %T", v)
		}
	}

	return ret, nil
}

func newValueListT(vl *api.ValueList) (*C.value_list_t, error) {
	ret := &C.value_list_t{}

	strcpy(ret.host[:], vl.Host)
	strcpy(ret.plugin[:], vl.Plugin)
	strcpy(ret.plugin_instance[:], vl.PluginInstance)
	strcpy(ret._type[:], vl.Type)
	strcpy(ret.type_instance[:], vl.TypeInstance)
	ret.interval = C.cdtime_t(cdtime.NewDuration(vl.Interval))
	ret.time = C.cdtime_t(cdtime.New(vl.Time))

	// metadata
	if len(vl.Metadata) > 0 {
		meta, err := newMetaDataT(vl.Metadata)
		if err != nil {
			return nil, fmt.Errorf("building metadata: %v", err)
		}
		ret.meta = meta
	}

	for _, v := range vl.Values {
		switch v := v.(type) {
		case api.Counter:
			if _, err := C.value_list_add_counter(ret, C.counter_t(v)); err != nil {
				return nil, fmt.Errorf("value_list_add_counter: %v", err)
			}
		case api.Derive:
			if _, err := C.value_list_add_derive(ret, C.derive_t(v)); err != nil {
				return nil, fmt.Errorf("value_list_add_derive: %v", err)
			}
		case api.Gauge:
			if _, err := C.value_list_add_gauge(ret, C.gauge_t(v)); err != nil {
				return nil, fmt.Errorf("value_list_add_gauge: %v", err)
			}
		default:
			return nil, fmt.Errorf("not yet supported: %T", v)
		}
	}

	return ret, nil
}

// writer implements the api.Write interface.
type writer struct{}

// NewWriter returns an object implementing the api.Writer interface for the
// collectd daemon.
func NewWriter() api.Writer {
	return writer{}
}

// Write implements the api.Writer interface for the collectd daemon.
func (writer) Write(_ context.Context, vl *api.ValueList) error {
	return Write(vl)
}

// Write converts a ValueList and calls the plugin_dispatch_values() function
// of the collectd daemon.
func Write(vl *api.ValueList) error {
	vlt, err := newValueListT(vl)
	if err != nil {
		return err
	}
	defer func() {
		if vlt.meta != nil {
			C.free(unsafe.Pointer(vlt.meta))
		}
		C.free(unsafe.Pointer(vlt.values))
	}()

	status, err := C.dispatch_values_wrapper(vlt)
	if err != nil {
		return err
	} else if status != 0 {
		return fmt.Errorf("dispatch_values failed with status %d", status)
	}

	return nil
}

// readFuncs holds references to all read callbacks, so the garbage collector
// doesn't get any funny ideas.
var readFuncs = make(map[string]Reader)

// ComplexReadConfig represents the extra configuration settings available
// in the RegisterComplexRead function
// See C function `plugin_register_complex_read` for more informations
type ComplexReadConfig struct {
	// See C function `plugin_unregister_read_group` for more informations
	// Defaults to defaultReadCallbackGroup
	Group string

	// Interval sets the interval in which to query the read plugin
	Interval time.Duration
}

func registerComplexRead(name string, r Reader, config ComplexReadConfig) error {
	interval := uint64(config.Interval.Seconds())

	var group string
	if config.Group == "" {
		group = defaultReadCallbackGroup
	} else {
		group = config.Group
	}

	cGroup := C.CString(group)
	defer C.free(unsafe.Pointer(cGroup))

	cName := C.CString(name)
	ud := C.user_data_t{
		data:      unsafe.Pointer(cName),
		free_func: nil,
	}

	status, err := C.register_read_wrapper(cGroup, cName,
		C.plugin_read_cb(C.wrap_read_callback),
		C.cdtime_t(interval),
		&ud)
	if err != nil {
		return err
	} else if status != 0 {
		return fmt.Errorf("register_read_wrapper failed with status %d", status)
	}

	readFuncs[name] = r
	return nil
}

// RegisterRead registers a new read function with the daemon which is called
// periodically.
// It behaves like `RegisterComplexRead` with an empty ComplexReadConfig configuration
func RegisterRead(name string, r Reader) error {
	return registerComplexRead(name, r, ComplexReadConfig{})
}

// RegisterComplexRead registers a new read function with the daemon which is called
// periodically.
// It gives you more control that the simple `RegisterRead` function in the way that
// you can specify the `Interval` between two calls to your function.
// You can also specify a custom callback group name (`Group`)
// See C function `plugin_register_complex_read` for more informations
func RegisterComplexRead(name string, r Reader, config ComplexReadConfig) error {
	return registerComplexRead(name, r, config)
}

//export wrap_read_callback
func wrap_read_callback(ud *C.user_data_t) C.int {
	name := C.GoString((*C.char)(ud.data))
	r, ok := readFuncs[name]
	if !ok {
		return -1
	}

	if err := r.Read(); err != nil {
		Errorf("%s plugin: Read() failed: %v", name, err)
		return -1
	}

	return 0
}

// writeFuncs holds references to all write callbacks, so the garbage collector
// doesn't get any funny ideas.
var writeFuncs = make(map[string]api.Writer)

// RegisterWrite registers a new write function with the daemon which is called
// for every metric collected by collectd.
//
// Please note that multiple threads may call this function concurrently. If
// you're accessing shared resources, such as a memory buffer, you have to
// implement appropriate locking around these accesses.
func RegisterWrite(name string, w api.Writer) error {
	cName := C.CString(name)
	ud := C.user_data_t{
		data:      unsafe.Pointer(cName),
		free_func: nil,
	}

	status, err := C.register_write_wrapper(cName, C.plugin_write_cb(C.wrap_write_callback), &ud)
	if err != nil {
		return err
	} else if status != 0 {
		return fmt.Errorf("register_write_wrapper failed with status %d", status)
	}

	writeFuncs[name] = w
	return nil
}

//export wrap_write_callback
func wrap_write_callback(ds *C.data_set_t, cvl *C.value_list_t, ud *C.user_data_t) C.int {
	name := C.GoString((*C.char)(ud.data))
	w, ok := writeFuncs[name]
	if !ok {
		return -1
	}

	vl := &api.ValueList{
		Identifier: api.Identifier{
			Host:           C.GoString(&cvl.host[0]),
			Plugin:         C.GoString(&cvl.plugin[0]),
			PluginInstance: C.GoString(&cvl.plugin_instance[0]),
			Type:           C.GoString(&cvl._type[0]),
			TypeInstance:   C.GoString(&cvl.type_instance[0]),
		},
		Time:     cdtime.Time(cvl.time).Time(),
		Interval: cdtime.Time(cvl.interval).Duration(),
	}

	// TODO: Remove 'size_t' cast on 'ds_num' upon 5.7 release.
	for i := C.size_t(0); i < C.size_t(ds.ds_num); i++ {
		dsrc := C.ds_dsrc(ds, i)

		switch dsrc._type {
		case C.DS_TYPE_COUNTER:
			v := C.value_list_get_counter(cvl, i)
			vl.Values = append(vl.Values, api.Counter(v))
		case C.DS_TYPE_DERIVE:
			v := C.value_list_get_derive(cvl, i)
			vl.Values = append(vl.Values, api.Derive(v))
		case C.DS_TYPE_GAUGE:
			v := C.value_list_get_gauge(cvl, i)
			vl.Values = append(vl.Values, api.Gauge(v))
		default:
			Errorf("%s plugin: data source type %d is not supported", name, dsrc._type)
			return -1
		}

		vl.DSNames = append(vl.DSNames, C.GoString(&dsrc.name[0]))
	}

	if err := w.Write(ctx, vl); err != nil {
		Errorf("%s plugin: Write() failed: %v", name, err)
		return -1
	}

	return 0
}

// First declare some types, interfaces, general functions

// Shutters are objects that when called will shut down the plugin gracefully
type Shutter interface {
	Shutdown() error
}

// shutdownFuncs holds references to all shutdown callbacks
var shutdownFuncs = make(map[string]Shutter)

//export wrap_shutdown_callback
func wrap_shutdown_callback() C.int {
	if len(shutdownFuncs) <= 0 {
		return 0
	}
	for n, s := range shutdownFuncs {
		if err := s.Shutdown(); err != nil {
			Errorf("%s plugin: Shutdown() failed: %v", n, s)
			return -1
		}
	}
	return 0
}

// RegisterShutdown registers a shutdown function with the daemon which is called
// when the plugin is required to shutdown gracefully.
func RegisterShutdown(name string, s Shutter) error {
	// Only register a callback the first time one is implemented, subsequent
	// callbacks get added to a list and called at the same time
	if len(shutdownFuncs) <= 0 {
		cName := C.CString(name)
		cCallback := C.plugin_shutdown_cb(C.wrap_shutdown_callback)

		status, err := C.register_shutdown_wrapper(cName, cCallback)
		if err != nil {
			Errorf("register_shutdown_wrapper failed with status: %v", status)
			return err
		}
	}
	shutdownFuncs[name] = s
	return nil
}

//export module_register
func module_register() {
}
