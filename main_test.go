package main_test

import (
	"testing"

	m "github.com/HSMF/minify-gobra"
)

type cs struct {
	inpt     string
	expected bool
}

func TestIsAssert(t *testing.T) {

	cases := []cs{
		{"//@assert true", true},
		{"       //@assert true", true},
		{"       //    @assert true", true},
		{"   // @assert true", true},
		{"// // @assert true", false},
		{" // @ assert true", true},
		{"// // #assert true", false},
		{"// #assert true", false},
		{"//#assert true", false},
		{"assert", false},
		{"//chop! //@assert true", false},
	}

	for _, cs := range cases {
		if (m.IsGobraComment(cs.inpt) && m.IsAssert(cs.inpt)) != cs.expected {
			t.Fatalf("IsAssert(%v) != %v", cs.inpt, cs.expected)
		}
	}

}
