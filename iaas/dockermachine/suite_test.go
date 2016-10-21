// Copyright 2016 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package dockermachine

import (
	"encoding/json"
	"errors"
	"io/ioutil"
	"os"
	"testing"

	check "gopkg.in/check.v1"

	"github.com/docker/machine/drivers/amazonec2"
	"github.com/docker/machine/drivers/fakedriver"
	"github.com/docker/machine/libmachine/auth"
	"github.com/docker/machine/libmachine/drivers"
	"github.com/docker/machine/libmachine/engine"
	"github.com/docker/machine/libmachine/host"
	"github.com/docker/machine/libmachine/persist/persisttest"
	"github.com/docker/machine/libmachine/state"
	"github.com/tsuru/tsuru/iaas"
)

func Test(t *testing.T) { check.TestingT(t) }

type S struct{}

var _ = check.Suite(&S{})

type fakeLibMachineAPI struct {
	*persisttest.FakeStore
	driverName string
	ec2Driver  *amazonec2.Driver
	closed     bool
	tempFiles  []*os.File
}

func (f *fakeLibMachineAPI) NewHost(driverName string, rawDriver []byte) (*host.Host, error) {
	f.driverName = driverName
	var driverOpts map[string]interface{}
	json.Unmarshal(rawDriver, &driverOpts)
	var driver drivers.Driver
	if driverName == "amazonec2" {
		driver = amazonec2.NewDriver("", "")
	} else {
		driver = &fakedriver.Driver{}
	}
	var name string
	if m, ok := driverOpts["MachineName"]; ok {
		name = m.(string)
	} else {
		name = driverOpts["MockName"].(string)
	}
	caFile, err := createTempFile("ca")
	if err != nil {
		return nil, err
	}
	certFile, err := createTempFile("cert")
	if err != nil {
		return nil, err
	}
	keyFile, err := createTempFile("key")
	if err != nil {
		return nil, err
	}
	f.tempFiles = append(f.tempFiles, caFile, certFile, keyFile)

	return &host.Host{
		Name:   name,
		Driver: driver,
		HostOptions: &host.Options{
			EngineOptions: &engine.Options{},
			AuthOptions: &auth.Options{
				CaCertPath:     caFile.Name(),
				ClientCertPath: certFile.Name(),
				ClientKeyPath:  keyFile.Name(),
			},
		},
	}, nil
}
func createTempFile(content string) (*os.File, error) {
	file, err := ioutil.TempFile("", "")
	if err != nil {
		return nil, err
	}
	_, err = file.WriteString(content)
	if err != nil {
		return nil, err
	}
	return file, nil
}

func (f *fakeLibMachineAPI) Create(h *host.Host) error {
	if f.driverName == "amazonec2" {
		f.ec2Driver = h.Driver.(*amazonec2.Driver)
	}
	h.Driver = &fakedriver.Driver{
		MockName:  h.Name,
		MockState: state.Running,
		MockIP:    "192.168.10.3",
	}
	if f.FakeStore == nil {
		f.FakeStore = &persisttest.FakeStore{
			Hosts: make([]*host.Host, 0),
		}
	}
	f.Save(h)
	return nil
}

func (f *fakeLibMachineAPI) Close() error {
	for _, f := range f.tempFiles {
		os.Remove(f.Name())
	}
	f.closed = true
	return nil
}

func (f *fakeLibMachineAPI) GetMachinesDir() string {
	return ""
}

type fakeDockerMachine struct {
	closed         bool
	deletedMachine *iaas.Machine
	createdMachine *Machine
	config         DockerMachineConfig
	hostOpts       CreateMachineOpts
}

var fakeDM = &fakeDockerMachine{}

func newFakeDockerMachine(c DockerMachineConfig) (DockerMachineAPI, error) {
	fakeDM.deletedMachine = nil
	fakeDM.createdMachine = nil
	fakeDM.config = c
	fakeDM.closed = false
	return fakeDM, nil
}

func (f *fakeDockerMachine) Close() error {
	f.closed = true
	return nil
}

func (f *fakeDockerMachine) CreateMachine(opts CreateMachineOpts) (*Machine, error) {
	f.createdMachine = &Machine{
		Base: &iaas.Machine{
			Id: opts.Name,
		},
	}
	var errCreate error
	if v, ok := opts.Params["error"]; ok {
		errCreate = errors.New(v.(string))
	}
	f.hostOpts = opts
	return f.createdMachine, errCreate
}

func (f *fakeDockerMachine) DeleteMachine(m *iaas.Machine) error {
	f.deletedMachine = m
	return nil
}