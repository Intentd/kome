// Package transitional makes the schema a field migration runs under.
// Kubernetes drops fields a schema does not declare, so while objects move
// from old paths to new ones the stored schema must declare both. Generate
// builds it from the destination schema.
package transitional

import (
	"fmt"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"

	"github.com/intentd/tools/kube"
)

// Move is what Generate needs of a field move: its old path and the paths it
// writes, relative to spec.
type Move struct {
	From string
	To   []string
}

// Generate returns the transitional schema of a field migration from the CRD
// files fromFile and toFile:
//
//  1. it starts from to's CRD: its printer columns, validation and storage
//     version are what objects end up with;
//  2. it adds every from path to lacks, with its type and description from
//     from;
//  3. it makes the to paths optional, since objects lack them until migrated,
//     and a path whose type changes in place holds either while objects move;
//  4. it shows each moved path's printer column from from beside the column
//     of its destination, so objects not yet migrated still show their value
//     (see moveColumns);
//  5. it refuses when the storage versions differ: that is a conversion.
func Generate(fromFile, toFile string, moves []Move) ([]byte, error) {
	from, err := kube.ReadCRDFile(fromFile)
	if err != nil {
		return nil, err
	}
	to, err := kube.ReadCRDFile(toFile)
	if err != nil {
		return nil, err
	}
	fromV, toV := kube.StorageVersion(from), kube.StorageVersion(to)
	if fromV == nil || toV == nil {
		return nil, fmt.Errorf("both schemas need a storage version")
	}
	if fromV.Name != toV.Name {
		return nil, fmt.Errorf("%s stores %s and %s stores %s: changing the storage version is a conversion, not a field move", fromFile, fromV.Name, toFile, toV.Name)
	}
	fromSpec, toSpec := kube.SpecSchema(fromV), kube.SpecSchema(toV)
	if toSpec == nil {
		return nil, fmt.Errorf("%s has no spec schema", toFile)
	}

	for _, m := range moves {
		src := kube.SchemaAtV1(fromSpec, m.From)
		switch dst := kube.SchemaAtV1(toSpec, m.From); {
		case !kube.SchemaHoldsV1(toSpec, m.From):
			if src == nil {
				return nil, fmt.Errorf("spec.%s is not in %s", m.From, fromFile)
			}
			kube.PutSchemaV1(toSpec, m.From, *src)
		case dst != nil && src != nil && dst.Type != src.Type:
			// The value changes shape in place (a fanout into its own key):
			// while objects move, the path holds either.
			kube.PutSchemaV1(toSpec, m.From, eitherShape(*src, *dst))
		}
		for _, p := range m.To {
			kube.MakeOptionalV1(toSpec, p)
		}
	}
	kube.SetSpecSchema(toV, *toSpec)
	toV.AdditionalPrinterColumns = moveColumns(fromV.AdditionalPrinterColumns, toV.AdditionalPrinterColumns, moves)
	to.Status = apiextensionsv1.CustomResourceDefinitionStatus{}
	to.CreationTimestamp.Reset()
	return yaml.Marshal(to)
}

// moveColumns returns to's printer columns with, for each move, from's
// column on its old path, unless to shows that path itself. Each goes just
// before the first to column on one of the move's destinations, else before
// the age column, else last. A name to already uses (kubectl shows names
// upper-cased) gets an Old prefix: to's names are the ones that stay.
func moveColumns(from, to []apiextensionsv1.CustomResourceColumnDefinition, moves []Move) []apiextensionsv1.CustomResourceColumnDefinition {
	out := append([]apiextensionsv1.CustomResourceColumnDefinition(nil), to...)
	shows := func(path string) bool {
		for _, c := range out {
			if c.JSONPath == path {
				return true
			}
		}
		return false
	}
	named := func(name string) bool {
		for _, c := range out {
			if strings.EqualFold(c.Name, name) {
				return true
			}
		}
		return false
	}
	for _, m := range moves {
		old := ".spec." + m.From
		if shows(old) {
			continue
		}
		for _, c := range from {
			if c.JSONPath != old {
				continue
			}
			if named(c.Name) {
				c.Name = "Old" + c.Name
			}
			at := columnBefore(out, m.To)
			out = append(out[:at], append([]apiextensionsv1.CustomResourceColumnDefinition{c}, out[at:]...)...)
			break
		}
	}
	return out
}

// columnBefore is the index a moved path's old column goes at in cols: that
// of the first column on one of dests, else of the age column, else the end.
func columnBefore(cols []apiextensionsv1.CustomResourceColumnDefinition, dests []string) int {
	for i, c := range cols {
		for _, d := range dests {
			if c.JSONPath == ".spec."+d {
				return i
			}
		}
	}
	for i, c := range cols {
		if c.JSONPath == ".metadata.creationTimestamp" {
			return i
		}
	}
	return len(cols)
}

// eitherShape is a schema that holds a value of either schema: typeless and
// open, which a structural schema allows for exactly this.
func eitherShape(old, new apiextensionsv1.JSONSchemaProps) apiextensionsv1.JSONSchemaProps {
	yes := true
	desc := new.Description
	if desc == "" {
		desc = old.Description
	}
	return apiextensionsv1.JSONSchemaProps{
		Description:            fmt.Sprintf("%s (%s while migrating, then %s)", strings.TrimSpace(desc), old.Type, new.Type),
		XPreserveUnknownFields: &yes,
	}
}
