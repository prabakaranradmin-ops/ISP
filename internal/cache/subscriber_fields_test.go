package cache

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/nas"
	"github.com/maaransoft/isp-bss-oss/internal/radius"
)

// Every field of radius.Subscriber survives a cache hit.
//
// The cache stores its own copy of the struct (cachedSubscriber) and copies
// field by field in both directions, so a field added to radius.Subscriber
// and not to the cache reads as its zero value on every hit: silently, for
// up to a minute after each login. That has happened to the speed override,
// the NT hash, the static address and the bandwidth profile (see the
// comments on cachedSubscriber), and would have happened to the connection
// limit, where a zero reads as one and refuses a paying subscriber's second
// connection. This fills every field by reflection, so a new one is covered
// without anyone remembering to add it here.

type oneSubscriberDB struct {
	sub   *radius.Subscriber
	calls int
}

func (d *oneSubscriberDB) GetSubscriberByUsername(context.Context, string) (*radius.Subscriber, error) {
	d.calls++
	cp := *d.sub
	return &cp, nil
}

func filledSubscriber(t *testing.T) *radius.Subscriber {
	t.Helper()
	sub := &radius.Subscriber{}
	v := reflect.ValueOf(sub).Elem()
	expires := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < v.NumField(); i++ {
		f, name := v.Field(i), v.Type().Field(i).Name
		switch f.Kind() {
		case reflect.String:
			f.SetString("value-of-" + name)
		case reflect.Int, reflect.Int64:
			f.SetInt(int64(7 + i))
		case reflect.Bool:
			f.SetBool(true)
		case reflect.Slice:
			f.Set(reflect.ValueOf([]byte("nt-hash-" + name)))
		case reflect.Pointer:
			switch f.Type() {
			case reflect.TypeOf(&expires):
				f.Set(reflect.ValueOf(&expires))
			case reflect.TypeOf(&nas.Shaping{}):
				f.Set(reflect.ValueOf(&nas.Shaping{Name: "burst", MaxDownBPS: 50_000_000, MaxUpBPS: 20_000_000}))
			default:
				t.Fatalf("radius.Subscriber.%s is a %s this test does not know how to fill; teach it", name, f.Type())
			}
		default:
			t.Fatalf("radius.Subscriber.%s is a %s this test does not know how to fill; teach it", name, f.Kind())
		}
	}
	return sub
}

func TestEverySubscriberFieldSurvivesACacheHit(t *testing.T) {
	want := filledSubscriber(t)
	db := &oneSubscriberDB{sub: want}
	c := NewSubscriberCache(db, time.Minute)

	if _, err := c.GetSubscriberByUsername(context.Background(), want.Username); err != nil {
		t.Fatal(err)
	}
	got, err := c.GetSubscriberByUsername(context.Background(), want.Username)
	if err != nil {
		t.Fatal(err)
	}
	if db.calls != 1 {
		t.Fatalf("the second read went to the database (%d calls); this is not testing a hit", db.calls)
	}
	gv, wv := reflect.ValueOf(got).Elem(), reflect.ValueOf(want).Elem()
	for i := 0; i < wv.NumField(); i++ {
		name := wv.Type().Field(i).Name
		if !reflect.DeepEqual(gv.Field(i).Interface(), wv.Field(i).Interface()) {
			t.Errorf("radius.Subscriber.%s is lost on a cache hit: got %v, want %v — add it to cachedSubscriber",
				name, gv.Field(i).Interface(), wv.Field(i).Interface())
		}
	}
}
