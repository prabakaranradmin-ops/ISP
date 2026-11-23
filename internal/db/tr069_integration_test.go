//go:build integration

// TR-069 persistence tests — FR-CPE-001..003 | MDS §4.19.
package db_test

import (
	"context"
	"sync"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/inventory"
	"github.com/maaransoft/isp-bss-oss/internal/tr069"
)

func informFor(serial string, events ...string) *tr069.Inform {
	evs := make([]tr069.EventStruct, 0, len(events))
	for _, e := range events {
		evs = append(evs, tr069.EventStruct{EventCode: e})
	}
	return &tr069.Inform{
		DeviceID: tr069.DeviceID{
			Manufacturer: "TP-Link", OUI: "001122",
			ProductClass: "ArcherC6", SerialNumber: serial,
		},
		Event: evs,
		ParameterList: []tr069.ParameterValue{
			{Name: "InternetGatewayDevice.DeviceInfo.SoftwareVersion", Value: "1.4.2"},
			{Name: "InternetGatewayDevice.ManagementServer.ConnectionRequestURL", Value: "http://10.0.0.1:7547/"},
		},
	}
}

// TestFR_CPE_001_InformFromAKnownDeviceUpdatesItInPlace: a warehouse device
// that starts talking CWMP must stay the same row, not become a second one.
func TestFR_CPE_001_InformFromAKnownDeviceUpdatesItInPlace(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	inv := database.Inventory()
	dt, err := inv.CreateDeviceType(ctx, inventory.DeviceType{Name: "Archer C6", VendorID: seedVendorID(ctx, t, pool, "TP-Link")})
	if err != nil {
		t.Fatalf("CreateDeviceType: %v", err)
	}
	if _, err := inv.CreateDevice(ctx, inventory.Device{DeviceTypeID: dt.ID, SerialNumber: "SN-KNOWN"}); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}

	store := database.TR069()
	device, err := store.UpsertDeviceFromInform(ctx, informFor("SN-KNOWN", tr069.EventBoot))
	if err != nil {
		t.Fatalf("UpsertDeviceFromInform: %v", err)
	}

	if device.ACSDiscovered {
		t.Error("a device that was in the warehouse must not be flagged ACS-discovered")
	}
	if device.SoftwareVersion != "1.4.2" {
		t.Errorf("software version = %q, want 1.4.2", device.SoftwareVersion)
	}
	if device.ConnectionRequestURL == "" {
		t.Error("the connection request URL from the Inform must be recorded")
	}
	if device.LastInformAt == nil {
		t.Error("contact must be timestamped")
	}
	// registered, because it was at 'unknown' before first contact.
	if device.ProvisioningState != tr069.StateRegistered {
		t.Errorf("provisioning_state = %q, want registered", device.ProvisioningState)
	}

	count := countRows(ctx, t, pool, `SELECT COUNT(*) FROM cpe_devices`)
	if count != 1 {
		t.Errorf("an Inform from a known device must not create a second row, got %d", count)
	}
}

// TestFR_CPE_001_InformFromAnUnknownDeviceIsDiscoveredNotStock: a field swap
// still has to be managed, but it never passed stock control — counting it
// as warehouse stock would corrupt FR-INV-003's reorder numbers.
func TestFR_CPE_001_InformFromAnUnknownDeviceIsDiscoveredNotStock(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	if _, err := database.Inventory().CreateDeviceType(ctx,
		inventory.DeviceType{Name: "Archer C6", VendorID: seedVendorID(ctx, t, pool, "TP-Link")}); err != nil {
		t.Fatalf("CreateDeviceType: %v", err)
	}

	device, err := database.TR069().UpsertDeviceFromInform(ctx, informFor("SN-STRANGER", tr069.EventBootstrap))
	if err != nil {
		t.Fatalf("UpsertDeviceFromInform: %v", err)
	}

	if !device.ACSDiscovered {
		t.Error("a device with no warehouse record must be flagged ACS-discovered")
	}

	// The schema forbids an ACS-discovered device from sitting in stock.
	levels, err := database.Inventory().GetStockLevels(ctx, false)
	if err != nil {
		t.Fatalf("GetStockLevels: %v", err)
	}
	for _, l := range levels {
		if l.InStock != 0 {
			t.Errorf("an ACS-discovered device must not count as stock, in_stock=%d", l.InStock)
		}
	}
}

// TestFR_CPE_001_RoutineInformDoesNotUndoProvisioning: a periodic check-in
// must not walk a provisioned device back to 'registered' and trigger a
// re-push every fifteen minutes.
func TestFR_CPE_001_RoutineInformDoesNotUndoProvisioning(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	if _, err := database.Inventory().CreateDeviceType(ctx,
		inventory.DeviceType{Name: "Archer C6", VendorID: seedVendorID(ctx, t, pool, "TP-Link")}); err != nil {
		t.Fatalf("CreateDeviceType: %v", err)
	}
	store := database.TR069()
	device, err := store.UpsertDeviceFromInform(ctx, informFor("SN-PROV", tr069.EventBootstrap))
	if err != nil {
		t.Fatalf("UpsertDeviceFromInform: %v", err)
	}
	if err := store.SetProvisioningState(ctx, device.ID, tr069.StateProvisioned, ""); err != nil {
		t.Fatalf("SetProvisioningState: %v", err)
	}

	again, err := store.UpsertDeviceFromInform(ctx, informFor("SN-PROV", tr069.EventPeriodic))
	if err != nil {
		t.Fatalf("second Inform: %v", err)
	}
	if again.ProvisioningState != tr069.StateProvisioned {
		t.Errorf("state = %q, want it to stay provisioned across a routine Inform", again.ProvisioningState)
	}
}

// TestFR_CPE_003_ConcurrentClaimsDeliverATaskExactlyOnce is the race the
// conditional claim exists for. A reboot delivered twice is a second,
// unexplained outage.
func TestFR_CPE_003_ConcurrentClaimsDeliverATaskExactlyOnce(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	if _, err := database.Inventory().CreateDeviceType(ctx,
		inventory.DeviceType{Name: "Archer C6", VendorID: seedVendorID(ctx, t, pool, "TP-Link")}); err != nil {
		t.Fatalf("CreateDeviceType: %v", err)
	}
	store := database.TR069()
	device, err := store.UpsertDeviceFromInform(ctx, informFor("SN-RACE", tr069.EventPeriodic))
	if err != nil {
		t.Fatalf("UpsertDeviceFromInform: %v", err)
	}
	if _, err := store.EnqueueTask(ctx, device.ID, tr069.RPCReboot, nil, 10, "noc1"); err != nil {
		t.Fatalf("EnqueueTask: %v", err)
	}

	const racers = 10
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners int
	)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			task, err := store.ClaimNextTask(ctx, device.ID)
			if err != nil {
				t.Errorf("ClaimNextTask: %v", err)
				return
			}
			if task != nil {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if winners != 1 {
		t.Errorf("DOUBLE DELIVERY: %d of %d concurrent claims got the same task, want exactly 1", winners, racers)
	}
}

// TestFR_CPE_003_TaskQueueOrderAndCompletion covers priority ordering and
// the outcome record.
func TestFR_CPE_003_TaskQueueOrderAndCompletion(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	if _, err := database.Inventory().CreateDeviceType(ctx,
		inventory.DeviceType{Name: "Archer C6", VendorID: seedVendorID(ctx, t, pool, "TP-Link")}); err != nil {
		t.Fatalf("CreateDeviceType: %v", err)
	}
	store := database.TR069()
	device, err := store.UpsertDeviceFromInform(ctx, informFor("SN-QUEUE", tr069.EventPeriodic))
	if err != nil {
		t.Fatalf("UpsertDeviceFromInform: %v", err)
	}

	// Enqueued low-priority first, so ordering by priority is what decides.
	if _, err := store.EnqueueTask(ctx, device.ID, tr069.RPCGetParameterValues,
		map[string]string{"0": "Device.X"}, 50, "noc1"); err != nil {
		t.Fatalf("EnqueueTask: %v", err)
	}
	urgent, err := store.EnqueueTask(ctx, device.ID, tr069.RPCReboot, nil, 10, "noc1")
	if err != nil {
		t.Fatalf("EnqueueTask: %v", err)
	}

	first, err := store.ClaimNextTask(ctx, device.ID)
	if err != nil {
		t.Fatalf("ClaimNextTask: %v", err)
	}
	if first == nil || first.ID != urgent.ID {
		t.Fatalf("want the higher-priority reboot first, got %+v", first)
	}
	if first.Status != tr069.TaskSent {
		t.Errorf("a claimed task must be marked sent, got %q", first.Status)
	}

	t.Run("params round trip", func(t *testing.T) {
		second, err := store.ClaimNextTask(ctx, device.ID)
		if err != nil {
			t.Fatalf("ClaimNextTask: %v", err)
		}
		if second.Params["0"] != "Device.X" {
			t.Errorf("params did not round trip: %+v", second.Params)
		}
	})

	t.Run("completion records the fault", func(t *testing.T) {
		if err := store.CompleteTask(ctx, first.ID, tr069.TaskFailed, "9003", "Invalid arguments"); err != nil {
			t.Fatalf("CompleteTask: %v", err)
		}
		tasks, err := store.ListTasks(ctx, device.ID, nil)
		if err != nil {
			t.Fatalf("ListTasks: %v", err)
		}
		var found bool
		for _, task := range tasks {
			if task.ID == first.ID {
				found = true
				if task.Status != tr069.TaskFailed || task.FaultCode != "9003" {
					t.Errorf("outcome not recorded: %+v", task)
				}
				if task.CompletedAt == nil {
					t.Error("a completed task must be timestamped")
				}
			}
		}
		if !found {
			t.Error("the task is missing from the history")
		}
	})

	t.Run("an empty queue returns (nil, nil)", func(t *testing.T) {
		task, err := store.ClaimNextTask(ctx, device.ID)
		if err != nil {
			t.Fatalf("ClaimNextTask on an empty queue must not error: %v", err)
		}
		if task != nil {
			t.Errorf("want nil, got %+v", task)
		}
	})
}

// TestFR_CPE_003_ExpiredTasksAreNotDelivered: a reboot queued a fortnight
// ago arriving now would be an unexplained outage.
func TestFR_CPE_003_ExpiredTasksAreNotDelivered(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	if _, err := database.Inventory().CreateDeviceType(ctx,
		inventory.DeviceType{Name: "Archer C6", VendorID: seedVendorID(ctx, t, pool, "TP-Link")}); err != nil {
		t.Fatalf("CreateDeviceType: %v", err)
	}
	store := database.TR069()
	device, err := store.UpsertDeviceFromInform(ctx, informFor("SN-STALE", tr069.EventPeriodic))
	if err != nil {
		t.Fatalf("UpsertDeviceFromInform: %v", err)
	}
	task, err := store.EnqueueTask(ctx, device.ID, tr069.RPCReboot, nil, 10, "noc1")
	if err != nil {
		t.Fatalf("EnqueueTask: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE cpe_tasks SET expires_at = NOW() - INTERVAL '1 day' WHERE id = $1`, task.ID); err != nil {
		t.Fatalf("age the task: %v", err)
	}

	claimed, err := store.ClaimNextTask(ctx, device.ID)
	if err != nil {
		t.Fatalf("ClaimNextTask: %v", err)
	}
	if claimed != nil {
		t.Error("an expired task must not be delivered")
	}
}

// TestFR_CPE_002_ProvisioningPlanDerivesFromThePlan is the requirement:
// CPE-side shaping comes from the same plan value that drives the NAS-side
// RADIUS limit.
func TestFR_CPE_002_ProvisioningPlanDerivesFromThePlan(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "TN_Super_100M", "100M/50M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "ravi@isp"})

	inv := database.Inventory()
	dt, err := inv.CreateDeviceType(ctx, inventory.DeviceType{Name: "Archer C6", VendorID: seedVendorID(ctx, t, pool, "TP-Link")})
	if err != nil {
		t.Fatalf("CreateDeviceType: %v", err)
	}
	// The template lives in data so a new router model is a row, not a release.
	if _, err := pool.Exec(ctx, `
		UPDATE cpe_device_types SET provisioning_template = $2::jsonb WHERE id = $1`,
		dt.ID, `{
			"Device.WiFi.SSID": "{{ssid}}",
			"Device.PPP.Username": "{{pppoe_username}}",
			"Device.PPP.Password": "{{pppoe_password}}",
			"Device.QoS.Up": "{{upstream_kbps}}",
			"Device.QoS.Down": "{{downstream_kbps}}"
		}`); err != nil {
		t.Fatalf("set template: %v", err)
	}
	if _, err := inv.CreateDevice(ctx, inventory.Device{DeviceTypeID: dt.ID, SerialNumber: "SN-PROVPLAN"}); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	if _, err := inv.IssueDevice(ctx, "SN-PROVPLAN", 1); err != nil {
		t.Fatalf("IssueDevice: %v", err)
	}

	device, err := database.TR069().GetDeviceBySerialACS(ctx, "SN-PROVPLAN")
	if err != nil || device == nil {
		t.Fatalf("GetDeviceBySerialACS: %v (%v)", err, device)
	}

	plan, err := database.TR069().GetProvisioningPlan(ctx, device.ID)
	if err != nil {
		t.Fatalf("GetProvisioningPlan: %v", err)
	}

	if plan["Device.PPP.Username"] != "ravi@isp" {
		t.Errorf("PPPoE username = %q, want the subscriber's", plan["Device.PPP.Username"])
	}
	// 100M/50M -> 100000 up, 50000 down, the same numbers the NAS shapes to.
	if plan["Device.QoS.Up"] != "100000" || plan["Device.QoS.Down"] != "50000" {
		t.Errorf("shaping = up %q / down %q, want 100000 / 50000",
			plan["Device.QoS.Up"], plan["Device.QoS.Down"])
	}
	// The safety property: bcrypt cannot yield a PPPoE password, and pushing
	// an empty one would disconnect the subscriber.
	if _, present := plan["Device.PPP.Password"]; present {
		t.Error("an unresolvable PPPoE password must be omitted, never pushed empty")
	}
}

// TestFR_CPE_002_UnissuedDeviceHasNothingToProvision: a warehouse device
// with no subscriber legitimately has no configuration, and that must not
// look like a failure.
func TestFR_CPE_002_UnissuedDeviceHasNothingToProvision(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	inv := database.Inventory()
	dt, err := inv.CreateDeviceType(ctx, inventory.DeviceType{Name: "Archer C6", VendorID: seedVendorID(ctx, t, pool, "TP-Link")})
	if err != nil {
		t.Fatalf("CreateDeviceType: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE cpe_device_types SET provisioning_template = '{"Device.WiFi.SSID":"{{ssid}}"}'::jsonb WHERE id = $1`,
		dt.ID); err != nil {
		t.Fatalf("set template: %v", err)
	}
	created, err := inv.CreateDevice(ctx, inventory.Device{DeviceTypeID: dt.ID, SerialNumber: "SN-SHELF"})
	if err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}

	plan, err := database.TR069().GetProvisioningPlan(ctx, created.ID)
	if err != nil {
		t.Fatalf("an unissued device must not error: %v", err)
	}
	if len(plan) != 0 {
		t.Errorf("want no parameters for a device with no subscriber, got %+v", plan)
	}
}
