// Package integration holds the cross-boundary assertions that no single
// package can make about itself.
//
// PRO-THESIS is split across two Go modules and four packages that are written
// and tested independently:
//
//	pkg/schema              the frozen wire contracts
//	internal/recorder       the codec, the clock, the seed streams, the artifacts
//	testdata/kvfixture      a SEPARATE module, standard-library only
//
// The fixture is the system under test. It is deliberately unable to import
// pkg/schema: a driver is an external, unmodified process (base directive §L),
// and letting the fixture link the harness's own types would make every
// conformance claim circular. The cost is that the history record shape, the
// oracle output shape and prothesis.yaml are declared TWICE, once in each
// module, and nothing in the type system keeps them in step.
//
// That is the gap these tests close. They read the fixture's committed artifacts
// as bytes and feed them to pkg/schema's decoders, which is exactly what the
// harness will do at run time. A drift between the two modules therefore fails
// here, at build time, rather than in Phase 3 as an unexplained INCONCLUSIVE.
//
// This package contains no production code and is imported by nothing.
package integration
