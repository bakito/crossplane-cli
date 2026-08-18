/*
Copyright 2026 The Crossplane Authors.

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

package generator

import (
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"sort"
	"strings"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
)

// Generated root types reference k8s.io/apimachinery. The runtime package is
// imported under the k8sruntime alias to avoid colliding with the
// github.com/oapi-codegen/runtime import the model files already carry.
const (
	roRuntimeAlias  = "k8sruntime"
	roRuntimeImport = "k8s.io/apimachinery/pkg/runtime"
	roSchemaImport  = "k8s.io/apimachinery/pkg/runtime/schema"
	roMetaAlias     = "k8smeta"
	roMetaImport    = "k8s.io/apimachinery/pkg/apis/meta/v1"
	roTypesAlias    = "k8stypes"
	roTypesImport   = "k8s.io/apimachinery/pkg/types"
)

// roScalarSelectorTypes are k8s types referenced via a package selector that are
// aliases to non-struct types (time.Time, maps) and therefore have no
// DeepCopyInto method; they must be copied by value, not deep-copied.
var roScalarSelectorTypes = map[string]bool{ //nolint:gochecknoglobals // Lookup table.
	"Time":         true, // k8smeta.Time / MicroTime alias time.Time
	"MicroTime":    true,
	"FieldsV1":     true, // alias for map[string]interface{}
	"RawExtension": true, // alias for a JSON-ish type, no DeepCopyInto
}

// applyRuntimeObjects runs addRuntimeObjects when enabled, discarding the
// root-types flag. It is called by the generation loops after all other Go
// post-processing so it operates on the final type names.
func applyRuntimeObjects(code string, enabled bool) (string, error) {
	if !enabled {
		return code, nil
	}
	out, _, err := addRuntimeObjects(code)
	return out, err
}

// addRuntimeObjects generates controller-gen-style DeepCopy methods for every
// struct in the given Go source, plus runtime.Object + schema.ObjectKind
// methods and a scheme-registering init() for every root type (a struct with
// both APIVersion and Kind fields). It returns the augmented source and whether
// any root types were found (so the caller can emit a groupversion_info.go for
// the package).
func addRuntimeObjects(code string) (string, bool, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", code, parser.ParseComments)
	if err != nil {
		return "", false, errors.Wrap(err, "failed to parse Go code for runtime.Object generation")
	}

	structs := collectStructTypes(f)
	aliases := collectCollectionAliases(f)

	// Identify metadata types used by root structs.
	metadataTypes := map[string]bool{"ObjectMeta": true}
	for name, st := range structs {
		if isRootStruct(st) && !hasField(st, "Items") && name != "Status" {
			metadataTypes[fieldElemTypeName(fset, st, "Metadata")] = true
		}
	}

	// Deterministic order: walk declarations in source order.
	var b strings.Builder
	hasRoots := false
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || ts.Assign.IsValid() {
				continue
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok || st.Fields == nil {
				continue
			}
			name := ts.Name.Name
			writeDeepCopy(&b, fset, name, st, structs, aliases)
			if isRootStruct(st) {
				hasRoots = true
				writeRuntimeObject(&b, fset, name, st)
				if !hasField(st, "Items") && name != "Status" {
					writeMetav1Object(&b, fset, name, st, true, structs)
				}
			} else if metadataTypes[name] {
				writeMetav1Object(&b, fset, name, st, false, structs)
			}
		}
	}

	if b.Len() == 0 {
		return code, false, nil
	}

	combined := code + "\n" + b.String()
	// DeepCopy methods need no imports; only root types reference runtime and
	// schema. Add the imports only when roots are present so DeepCopy-only files
	// (e.g. the shared k8s packages) don't get unused imports.
	if hasRoots {
		combined, err = ensureImports(combined, []importSpec{
			{alias: roRuntimeAlias, path: roRuntimeImport},
			{path: roSchemaImport},
			{alias: roMetaAlias, path: roMetaImport},
			{alias: roTypesAlias, path: roTypesImport},
		})
		if err != nil {
			return "", false, err
		}
	}

	formatted, err := format.Source([]byte(combined))
	if err != nil {
		return "", false, errors.Wrap(err, "failed to format generated runtime.Object code")
	}
	return string(formatted), hasRoots, nil
}

// collectStructTypes returns the set of struct type names declared in the file.
func collectStructTypes(f *ast.File) map[string]*ast.StructType {
	out := map[string]*ast.StructType{}
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || ts.Assign.IsValid() {
				continue
			}
			if st, ok := ts.Type.(*ast.StructType); ok {
				out[ts.Name.Name] = st
			}
		}
	}
	return out
}

// collectCollectionAliases returns local named types whose underlying type is a
// map or slice (both `type X map[..]` and `type X = map[..]`). Fields of these
// types must be deep-copied like a literal map/slice, not shallow-copied as a
// scalar.
func collectCollectionAliases(f *ast.File) map[string]ast.Expr {
	out := map[string]ast.Expr{}
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			switch ts.Type.(type) {
			case *ast.MapType, *ast.ArrayType:
				out[ts.Name.Name] = ts.Type
			}
		}
	}
	return out
}

// isRootStruct reports whether the struct is a top-level Kubernetes object: it
// has APIVersion, Kind and Metadata fields. The Metadata requirement excludes
// object references (claimRef, resourceRefs, ownerReferences, …) which also
// carry apiVersion/kind but are not registrable kinds.
func isRootStruct(st *ast.StructType) bool {
	hasAPIVersion, hasKind, hasMetadata := false, false, false
	for _, field := range st.Fields.List {
		for _, n := range field.Names {
			switch n.Name {
			case "APIVersion":
				hasAPIVersion = true
			case "Kind":
				hasKind = true
			case "Metadata":
				hasMetadata = true
			}
		}
	}
	return hasAPIVersion && hasKind && hasMetadata
}

// fieldKind classifies how a field's element type must be deep-copied.
type fieldKind int

const (
	// fkScalar: copy by value (basic types, named string enums, time.Time).
	fkScalar fieldKind = iota
	// fkStruct: a struct with a DeepCopyInto method.
	fkStruct
)

// classifyElem classifies the element type expr (the type with any leading
// pointer/slice/map already stripped) as scalar or struct.
func classifyElem(e ast.Expr, structs map[string]*ast.StructType) fieldKind {
	switch x := e.(type) {
	case *ast.Ident:
		if structs[x.Name] != nil {
			return fkStruct
		}
		// Basic types and named scalar (enum) types copy by value.
		return fkScalar
	case *ast.SelectorExpr:
		// pkg.Type — time.* and known alias types (Time, MicroTime, FieldsV1)
		// are scalars; every other referenced package type is a generated struct
		// with a DeepCopyInto method.
		if pkg, ok := x.X.(*ast.Ident); ok && pkg.Name == "time" {
			return fkScalar
		}
		if roScalarSelectorTypes[x.Sel.Name] {
			return fkScalar
		}
		return fkStruct
	default:
		return fkScalar
	}
}

// writeDeepCopy appends DeepCopyInto and DeepCopy methods for the struct.
func writeDeepCopy(b *strings.Builder, fset *token.FileSet, name string, st *ast.StructType, structs map[string]*ast.StructType, aliases map[string]ast.Expr) {
	fmt.Fprintf(b, "\n// DeepCopyInto copies the receiver into out.\n")
	fmt.Fprintf(b, "func (in *%s) DeepCopyInto(out *%s) {\n", name, name)
	b.WriteString("\t*out = *in\n")
	for _, field := range st.Fields.List {
		if len(field.Names) == 0 {
			continue
		}
		for _, n := range field.Names {
			writeFieldCopy(b, fset, n.Name, field.Type, structs, aliases)
		}
	}
	b.WriteString("}\n")

	fmt.Fprintf(b, "\n// DeepCopy returns a deep copy of the receiver.\n")
	fmt.Fprintf(b, "func (in *%s) DeepCopy() *%s {\n", name, name)
	b.WriteString("\tif in == nil {\n\t\treturn nil\n\t}\n")
	fmt.Fprintf(b, "\tout := new(%s)\n", name)
	b.WriteString("\tin.DeepCopyInto(out)\n\treturn out\n}\n")
}

// writeFieldCopy appends the deep-copy snippet for a single field. All generated
// fields are pointers; the leading pointer is handled here, then the pointee
// (scalar, struct, slice or map) is copied appropriately. Named aliases to a
// map or slice are deep-copied like their literal form.
func writeFieldCopy(b *strings.Builder, fset *token.FileSet, field string, typ ast.Expr, structs map[string]*ast.StructType, aliases map[string]ast.Expr) {
	star, ok := typ.(*ast.StarExpr)
	if !ok {
		// Non-pointer fields are copied by the `*out = *in` shallow assignment.
		// Generated models use pointers throughout, but guard defensively.
		return
	}

	fmt.Fprintf(b, "\tif in.%s != nil {\n", field)
	fmt.Fprintf(b, "\t\tin, out := &in.%s, &out.%s\n", field, field)

	pointee := star.X
	declType := renderType(fset, pointee)
	// Resolve a named alias (e.g. `type Patch = map[string]interface{}`) to its
	// underlying map/slice so it is deep-copied rather than shallow-copied.
	if id, ok := pointee.(*ast.Ident); ok {
		if underlying, isAlias := aliases[id.Name]; isAlias {
			pointee = underlying
		}
	}

	switch p := pointee.(type) {
	case *ast.ArrayType:
		writeSliceCopy(b, declType, p, structs)
	case *ast.MapType:
		writeMapCopy(b, fset, declType, p, structs)
	default:
		fmt.Fprintf(b, "\t\t*out = new(%s)\n", declType)
		if classifyElem(pointee, structs) == fkStruct {
			b.WriteString("\t\t(*in).DeepCopyInto(*out)\n")
		} else {
			b.WriteString("\t\t**out = **in\n")
		}
	}

	b.WriteString("\t}\n")
}

// writeSliceCopy handles a *[]Elem field. declType is the type to allocate (the
// literal slice type or a named alias). On entry in/out are *(*declType).
func writeSliceCopy(b *strings.Builder, declType string, arr *ast.ArrayType, structs map[string]*ast.StructType) {
	fmt.Fprintf(b, "\t\t*out = new(%s)\n", declType)
	b.WriteString("\t\tif *in != nil {\n")
	b.WriteString("\t\t\tin, out := *in, *out\n")
	fmt.Fprintf(b, "\t\t\t*out = make(%s, len(*in))\n", declType)
	if classifyElem(arr.Elt, structs) == fkStruct {
		b.WriteString("\t\t\tfor i := range *in {\n")
		b.WriteString("\t\t\t\t(*in)[i].DeepCopyInto(&(*out)[i])\n")
		b.WriteString("\t\t\t}\n")
	} else {
		b.WriteString("\t\t\tcopy(*out, *in)\n")
	}
	b.WriteString("\t\t}\n")
}

// writeMapCopy handles a *map[K]V field. declType is the type to allocate (the
// literal map type or a named alias). On entry in/out are *(*declType).
func writeMapCopy(b *strings.Builder, fset *token.FileSet, declType string, m *ast.MapType, structs map[string]*ast.StructType) {
	valType := renderType(fset, m.Value)
	fmt.Fprintf(b, "\t\t*out = new(%s)\n", declType)
	b.WriteString("\t\tif *in != nil {\n")
	b.WriteString("\t\t\tin, out := *in, *out\n")
	fmt.Fprintf(b, "\t\t\t*out = make(%s, len(*in))\n", declType)
	if classifyElem(m.Value, structs) == fkStruct {
		fmt.Fprintf(b, "\t\t\tfor key, val := range *in {\n")
		fmt.Fprintf(b, "\t\t\t\tvar v %s\n", valType)
		b.WriteString("\t\t\t\tval.DeepCopyInto(&v)\n")
		b.WriteString("\t\t\t\t(*out)[key] = v\n")
		b.WriteString("\t\t\t}\n")
	} else {
		b.WriteString("\t\t\tfor key, val := range *in {\n")
		b.WriteString("\t\t\t\t(*out)[key] = val\n")
		b.WriteString("\t\t\t}\n")
	}
	b.WriteString("\t\t}\n")
}

// writeRuntimeObject appends runtime.Object + schema.ObjectKind methods and a
// scheme-registering init() for a root type.
func writeRuntimeObject(b *strings.Builder, fset *token.FileSet, name string, st *ast.StructType) {
	// DeepCopyObject.
	fmt.Fprintf(b, "\n// DeepCopyObject returns a deep copy of the receiver as a runtime.Object.\n")
	fmt.Fprintf(b, "func (in *%s) DeepCopyObject() %s.Object {\n", name, roRuntimeAlias)
	b.WriteString("\tif c := in.DeepCopy(); c != nil {\n\t\treturn c\n\t}\n\treturn nil\n}\n")

	// GetObjectKind returns the object itself, which implements schema.ObjectKind.
	fmt.Fprintf(b, "\n// GetObjectKind implements runtime.Object.\n")
	fmt.Fprintf(b, "func (in *%s) GetObjectKind() schema.ObjectKind { return in }\n", name)

	apiVersionType := fieldElemTypeName(fset, st, "APIVersion")
	kindType := fieldElemTypeName(fset, st, "Kind")

	// GroupVersionKind reads the typed APIVersion/Kind fields.
	fmt.Fprintf(b, "\n// GroupVersionKind implements schema.ObjectKind.\n")
	fmt.Fprintf(b, "func (in *%s) GroupVersionKind() schema.GroupVersionKind {\n", name)
	b.WriteString("\tvar apiVersion, kind string\n")
	b.WriteString("\tif in.APIVersion != nil {\n\t\tapiVersion = string(*in.APIVersion)\n\t}\n")
	b.WriteString("\tif in.Kind != nil {\n\t\tkind = string(*in.Kind)\n\t}\n")
	b.WriteString("\treturn schema.FromAPIVersionAndKind(apiVersion, kind)\n}\n")

	// SetGroupVersionKind writes the typed APIVersion/Kind fields.
	fmt.Fprintf(b, "\n// SetGroupVersionKind implements schema.ObjectKind.\n")
	fmt.Fprintf(b, "func (in *%s) SetGroupVersionKind(gvk schema.GroupVersionKind) {\n", name)
	fmt.Fprintf(b, "\tapiVersion := %s(gvk.GroupVersion().String())\n", apiVersionType)
	b.WriteString("\tin.APIVersion = &apiVersion\n")
	fmt.Fprintf(b, "\tkind := %s(gvk.Kind)\n", kindType)
	b.WriteString("\tin.Kind = &kind\n}\n")

	// Register the type with the package SchemeBuilder (defined in
	// groupversion_info.go) so AddToScheme makes it known to a runtime.Scheme.
	fmt.Fprintf(b, "\nfunc init() {\n")
	fmt.Fprintf(b, "\tSchemeBuilder.Register(func(s *%s.Scheme) error {\n", roRuntimeAlias)
	fmt.Fprintf(b, "\t\ts.AddKnownTypes(GroupVersion, &%s{})\n", name)
	b.WriteString("\t\treturn nil\n\t})\n}\n")
}

// writeMetav1Object appends metav1.Object methods for a root type or its
// metadata field type.
func writeMetav1Object(b *strings.Builder, fset *token.FileSet, name string, st *ast.StructType, isRoot bool, structs map[string]*ast.StructType) {
	methods := []struct {
		method  string
		retType string
		args    string
		call    string
		field   string
		comment string
	}{
		{method: "GetNamespace", retType: "string", field: "Namespace", comment: "GetNamespace returns the namespace of the object."},
		{method: "SetNamespace", args: "namespace string", field: "Namespace", comment: "SetNamespace sets the namespace of the object."},
		{method: "GetName", retType: "string", field: "Name", comment: "GetName returns the name of the object."},
		{method: "SetName", args: "name string", field: "Name", comment: "SetName sets the name of the object."},
		{method: "GetGenerateName", retType: "string", field: "GenerateName", comment: "GetGenerateName returns the generate name of the object."},
		{method: "SetGenerateName", args: "name string", field: "GenerateName", comment: "SetGenerateName sets the generate name of the object."},
		{method: "GetUID", retType: roTypesAlias + ".UID", field: "UID", comment: "GetUID returns the UID of the object."},
		{method: "SetUID", args: "uid " + roTypesAlias + ".UID", field: "UID", comment: "SetUID sets the UID of the object."},
		{method: "GetResourceVersion", retType: "string", field: "ResourceVersion", comment: "GetResourceVersion returns the resource version of the object."},
		{method: "SetResourceVersion", args: "version string", field: "ResourceVersion", comment: "SetResourceVersion sets the resource version of the object."},
		{method: "GetGeneration", retType: "int64", field: "Generation", comment: "GetGeneration returns the generation of the object."},
		{method: "SetGeneration", args: "generation int64", field: "Generation", comment: "SetGeneration sets the generation of the object."},
		{method: "GetSelfLink", retType: "string", field: "SelfLink", comment: "GetSelfLink returns the self link of the object."},
		{method: "SetSelfLink", args: "selfLink string", field: "SelfLink", comment: "SetSelfLink sets the self link of the object."},
		{method: "GetCreationTimestamp", retType: roMetaAlias + ".Time", field: "CreationTimestamp", comment: "GetCreationTimestamp returns the creation timestamp of the object."},
		{method: "SetCreationTimestamp", args: "timestamp " + roMetaAlias + ".Time", field: "CreationTimestamp", comment: "SetCreationTimestamp sets the creation timestamp of the object."},
		{method: "GetDeletionTimestamp", retType: "*" + roMetaAlias + ".Time", field: "DeletionTimestamp", comment: "GetDeletionTimestamp returns the deletion timestamp of the object."},
		{method: "SetDeletionTimestamp", args: "timestamp *" + roMetaAlias + ".Time", field: "DeletionTimestamp", comment: "SetDeletionTimestamp sets the deletion timestamp of the object."},
		{method: "GetDeletionGracePeriodSeconds", retType: "*int64", field: "DeletionGracePeriodSeconds", comment: "GetDeletionGracePeriodSeconds returns the deletion grace period seconds of the object."},
		{method: "SetDeletionGracePeriodSeconds", args: "gracePeriodSeconds *int64", field: "DeletionGracePeriodSeconds", comment: "SetDeletionGracePeriodSeconds sets the deletion grace period seconds of the object."},
		{method: "GetLabels", retType: "map[string]string", field: "Labels", comment: "GetLabels returns the labels of the object."},
		{method: "SetLabels", args: "labels map[string]string", field: "Labels", comment: "SetLabels sets the labels of the object."},
		{method: "GetAnnotations", retType: "map[string]string", field: "Annotations", comment: "GetAnnotations returns the annotations of the object."},
		{method: "SetAnnotations", args: "annotations map[string]string", field: "Annotations", comment: "SetAnnotations sets the annotations of the object."},
		{method: "GetFinalizers", retType: "[]string", field: "Finalizers", comment: "GetFinalizers returns the finalizers of the object."},
		{method: "SetFinalizers", args: "finalizers []string", field: "Finalizers", comment: "SetFinalizers sets the finalizers of the object."},
		{method: "GetOwnerReferences", retType: "[]" + roMetaAlias + ".OwnerReference", field: "OwnerReferences", comment: "GetOwnerReferences returns the owner references of the object."},
		{method: "SetOwnerReferences", args: "refs []" + roMetaAlias + ".OwnerReference", field: "OwnerReferences", comment: "SetOwnerReferences sets the owner references of the object."},
		{method: "GetManagedFields", retType: "[]" + roMetaAlias + ".ManagedFieldsEntry", field: "ManagedFields", comment: "GetManagedFields returns the managed fields of the object."},
		{method: "SetManagedFields", args: "fields []" + roMetaAlias + ".ManagedFieldsEntry", field: "ManagedFields", comment: "SetManagedFields sets the managed fields of the object."},
	}

	for _, m := range methods {
		fmt.Fprintf(b, "\n// %s\n", m.comment)
		if m.retType != "" {
			fmt.Fprintf(b, "func (in *%s) %s() %s {\n", name, m.method, m.retType)
		} else {
			fmt.Fprintf(b, "func (in *%s) %s(%s) {\n", name, m.method, m.args)
		}

		if isRoot {
			b.WriteString("\tif in.Metadata == nil { return")
			if m.retType != "" {
				b.WriteString(" " + zeroValue(m.retType))
			}
			b.WriteString(" }\n")
			if m.retType != "" {
				fmt.Fprintf(b, "\treturn in.Metadata.%s()\n}\n", m.method)
			} else {
				argName := strings.Split(m.args, " ")[0]
				fmt.Fprintf(b, "\tin.Metadata.%s(%s)\n}\n", m.method, argName)
			}
			continue
		}

		// Non-root: access fields directly.
		hasField := hasField(st, m.field)
		if m.retType != "" {
			if !hasField {
				fmt.Fprintf(b, "\treturn %s\n}\n", zeroValue(m.retType))
				continue
			}
			// Special handling for different types.
			switch {
			case m.method == "GetLabels" || m.method == "GetAnnotations" || m.method == "GetFinalizers":
				b.WriteString("\tif in." + m.field + " == nil { return nil }\n")
				b.WriteString("\treturn *in." + m.field + "\n}\n")
			case strings.HasPrefix(m.retType, "[]") || strings.HasPrefix(m.retType, "*"):
				// Complex types (OwnerReferences, ManagedFields, DeletionTimestamp):
				// return nil unless the local type matches or we can convert it.
				// For now, we return nil to ensure it compiles if types differ.
				// In practice, these fields often have custom types in generated models.
				fmt.Fprintf(b, "\treturn nil // field %s present but type conversion not supported\n}\n", m.field)
			case m.retType == roMetaAlias+".Time":
				fmt.Fprintf(b, "\treturn %s.Time{} // field %s present but type conversion not supported\n}\n", roMetaAlias, m.field)
			default:
				b.WriteString("\tif in." + m.field + " == nil { return " + zeroValue(m.retType) + " }\n")
				fmt.Fprintf(b, "\treturn %s(*in.%s)\n}\n", m.retType, m.field)
			}
		} else {
			if !hasField {
				b.WriteString("}\n")
				continue
			}
			argName := strings.Split(m.args, " ")[0]
			argType := strings.Split(m.args, " ")[1]

			// Setters: only set if types match.
			fieldType := fieldElemTypeName(fset, st, m.field)

			// Check if we supported this type in Getter
			if strings.HasPrefix(m.retType, "[]") || strings.HasPrefix(m.retType, "*") || m.retType == roMetaAlias+".Time" {
				// For now, no-op for setters where type conversion is not supported
				fmt.Fprintf(b, "\t// field %s present but type conversion not supported\n}\n", m.field)
				continue
			}

			if (fieldType == "string" || fieldType == "int64") || argType == fieldType || argType == "*"+fieldType {
				if strings.HasPrefix(argType, "*") {
					fmt.Fprintf(b, "\tin.%s = %s\n}\n", m.field, argName)
				} else {
					argCast := argName
					if fieldType == "string" && argType != "string" {
						argCast = fmt.Sprintf("string(%s)", argName)
					} else if fieldType == "int64" && argType != "int64" {
						argCast = fmt.Sprintf("int64(%s)", argName)
					}
					fmt.Fprintf(b, "\tv := %s\n", argCast)
					fmt.Fprintf(b, "\tin.%s = &v\n}\n", m.field)
				}
			} else {
				fmt.Fprintf(b, "\t// field %s present but type %s not compatible with %s\n}\n", m.field, argType, fieldType)
			}
		}
	}
}

func zeroValue(t string) string {
	switch {
	case t == "string":
		return `""`
	case t == "int64":
		return "0"
	case strings.HasPrefix(t, "map["), strings.HasPrefix(t, "[]"), strings.HasPrefix(t, "*"):
		return "nil"
	case strings.Contains(t, roTypesAlias+".UID"):
		return `""`
	case strings.Contains(t, roMetaAlias+".Time"):
		return roMetaAlias + ".Time{}"
	default:
		return "nil"
	}
}

// hasField reports whether the struct has a field with the given name.
func hasField(st *ast.StructType, name string) bool {
	for _, field := range st.Fields.List {
		for _, n := range field.Names {
			if n.Name == name {
				return true
			}
		}
	}
	return false
}

// fieldElemTypeName returns the element type name of a pointer field (e.g. for
// `APIVersion *FooAPIVersion` it returns "FooAPIVersion"; for `*string` it
// returns "string"). Used to cast when setting the typed fields.
func fieldElemTypeName(fset *token.FileSet, st *ast.StructType, field string) string {
	for _, f := range st.Fields.List {
		for _, n := range f.Names {
			if n.Name != field {
				continue
			}
			if star, ok := f.Type.(*ast.StarExpr); ok {
				return renderType(fset, star.X)
			}
			return renderType(fset, f.Type)
		}
	}
	return "string"
}

// renderType renders a type expression to its Go source string.
func renderType(fset *token.FileSet, e ast.Expr) string {
	var sb strings.Builder
	if err := format.Node(&sb, fset, e); err != nil {
		return ""
	}
	return sb.String()
}

// importSpec describes an import to add: an optional alias and the package path.
type importSpec struct {
	alias string
	path  string
}

// ensureImports adds the given imports to the file if not already present,
// inserting them as a new import declaration after the package clause.
// format.Source tidies the result afterward.
func ensureImports(code string, specs []importSpec) (string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", code, parser.ParseComments)
	if err != nil {
		return "", errors.Wrap(err, "failed to parse code for import injection")
	}

	existing := map[string]bool{}
	for _, imp := range f.Imports {
		existing[strings.Trim(imp.Path.Value, `"`)] = true
	}

	var lines []string
	for _, s := range specs {
		if existing[s.path] {
			continue
		}
		if s.alias != "" {
			lines = append(lines, fmt.Sprintf("\t%s %q", s.alias, s.path))
		} else {
			lines = append(lines, fmt.Sprintf("\t%q", s.path))
		}
	}
	if len(lines) == 0 {
		return code, nil
	}
	sort.Strings(lines)

	imports := "import (\n" + strings.Join(lines, "\n") + "\n)\n"

	pkgEnd := fset.Position(f.Name.End()).Offset
	return code[:pkgEnd] + "\n\n" + imports + code[pkgEnd:], nil
}
