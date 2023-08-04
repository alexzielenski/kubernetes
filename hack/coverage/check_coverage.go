/*
Copyright 2023 The Kubernetes Authors.

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

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/build"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/google/go-cmp/cmp"
	"github.com/spf13/cobra"
	"golang.org/x/tools/cover"

	extensionsapiserver "k8s.io/apiextensions-apiserver/pkg/apiserver"
	aggregatorscheme "k8s.io/kube-aggregator/pkg/apiserver/scheme"
	"k8s.io/kube-openapi/pkg/spec3"
	"k8s.io/kube-openapi/pkg/validation/spec"
	"k8s.io/kubernetes/pkg/api/legacyscheme"

	// Initialize install packages
	_ "k8s.io/kubernetes/pkg/api/testing"

	// Not included in testing package. Should it be added?
	_ "k8s.io/kubernetes/pkg/apis/apiserverinternal/install"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

var schemes []*runtime.Scheme = []*runtime.Scheme{
	legacyscheme.Scheme, aggregatorscheme.Scheme, extensionsapiserver.Scheme,
}

// Should be run after OpenAPI spec has been generated.
//
// For each API type published in OpenAPI, the corresponding internal type
// should have a validate package rooted at the type's pkgPath/validation
//
// Running tests in that package should yield 100% coverage

func newCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "check_coverage <openapi-spec-dir> <apis_dir>",
		Short: `Checks validation tests in API packages should yield 100% coverage`,
		Run: func(cmd *cobra.Command, args []string) {
			if len(args) != 1 {
				cmd.Help()
				os.Exit(1)
			}

			openAPISpecPath := args[0]
			err := processSpec(openAPISpecPath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
			os.Exit(0)
		},
	}
	return cmd
}

func main() {
	command := newCommand()
	if err := command.Execute(); err != nil {
		os.Exit(1)
	}
}

func processSpec(openAPISpecDir string) error {
	// List directories for list of groups
	entries, err := os.ReadDir(openAPISpecDir)
	if err != nil {
		return err
	}

	suf := "_openapi.json"
	pre1 := "apis__"
	pre2 := "api__"

	groups := sets.Set[string]{}
	groupVersions := map[schema.GroupVersion]*spec3.OpenAPI{}
	groupVersionKinds := map[schema.GroupVersionKind]*spec.Schema{}
	for _, f := range entries {
		name := f.Name()

		if !strings.HasSuffix(name, suf) {
			continue
		} else if !strings.HasPrefix(name, pre1) && !strings.HasPrefix(name, pre2) {
			continue
		}

		trimmed := strings.TrimSuffix(name, suf)
		trimmed = strings.TrimPrefix(trimmed, pre1)
		trimmed = strings.TrimPrefix(trimmed, pre2)

		group, version, hasVersion := strings.Cut(trimmed, "__")
		if !hasVersion {
			if strings.HasPrefix(name, pre2) {
				version = group
				group = "core"
			} else {
				continue
			}
		}

		sch := spec3.OpenAPI{}
		schBytes, err := os.ReadFile(filepath.Join(openAPISpecDir, f.Name()))
		if err != nil {
			return err
		}

		if err := json.Unmarshal(schBytes, &sch); err != nil {
			return err
		}

		for _, d := range sch.Components.Schemas {
			var gvks []metav1.GroupVersionKind
			if err := d.Extensions.GetObject("x-kubernetes-group-version-kind", &gvks); err != nil {
				return err
			} else if len(gvks) == 0 {
				continue
			}

			for _, gvk := range gvks {
				var scheme *runtime.Scheme
				for _, s := range schemes {
					if s.IsVersionRegistered(schema.GroupVersion{
						Group:   gvk.Group,
						Version: gvk.Version,
					}) {
						scheme = s
						break
					}
				}
				if scheme == nil {
					continue
				}
				groupVersionKinds[schema.GroupVersionKind(gvk)] = d
				break
			}
		}

		tg := group
		if tg == "core" {
			tg = ""
		}
		groupVersions[schema.GroupVersion{
			Group:   tg,
			Version: version,
		}] = &sch
		groups.Insert(group)
	}

	grps := []string{}
	for g := range groups {
		if g == "core" {
			continue
		}
		grps = append(grps, g)
	}
	sort.Strings(grps)
	grps = append([]string{"core"}, grps...)

	sortedGroupVerisionKinds := []schema.GroupVersionKind{}
	for k := range groupVersionKinds {
		sortedGroupVerisionKinds = append(sortedGroupVerisionKinds, k)
	}
	sort.SliceStable(sortedGroupVerisionKinds, func(a, b int) bool {
		aGVK := sortedGroupVerisionKinds[a]
		bGVK := sortedGroupVerisionKinds[b]

		if aGVK.Group == "" {
			return true
		} else if bGVK.Group == "" {
			return false
		} else if aGVK.Group < bGVK.Group {
			return true
		} else if aGVK.Group > bGVK.Group {
			return false
		}

		if aGVK.Version < bGVK.Version {
			return true
		} else if aGVK.Version > bGVK.Version {
			return false
		}

		return aGVK.Kind < bGVK.Kind
	})

	hadError := false
	// for _, group := range grps {
	// 	tg := group
	// 	if tg == "core" {
	// 		tg = ""
	// 	}

	// 	var allTypes []reflect.Type
	// 	for _, s := range schemes {
	// 		types := s.KnownTypes(schema.GroupVersion{
	// 			Group:   tg,
	// 			Version: runtime.APIVersionInternal,
	// 		})

	// 		for _, t := range types {
	// 			allTypes = append(allTypes, t)
	// 		}
	// 	}

	// 	if len(allTypes) == 0 {
	// 		// shouldnt happen
	// 		fmt.Printf("failed to resolve a type: %v\n", group)
	// 		continue
	// 	}

	// 	var dir *build.Package
	// 	for _, v := range allTypes {
	// 		typesPackage := v.PkgPath()
	// 		if strings.HasSuffix(typesPackage, "meta/v1") {
	// 			continue
	// 		}
	// 		// Resolve the go package filename to source
	// 		dir, err = build.Default.Import(typesPackage, ".", build.FindOnly)
	// 		if err != nil {
	// 			continue
	// 		}
	// 		break
	// 	}

	// 	if dir == nil {
	// 		return errors.New("failed to find source for internal type")
	// 	}

	// 	if errs := checkValidationForGroup(group, dir.Dir); len(errs) > 0 {
	// 		hadError = true
	// 		fmt.Printf("- [ ] %v\n", group)
	// 		for _, e := range errs {
	// 			fmt.Printf("    - %v\n", e.Error())
	// 		}
	// 	} else {
	// 		fmt.Printf("- [x] %v\n", group)
	// 	}
	// }

	for _, gvk := range sortedGroupVerisionKinds {
		if strings.HasSuffix(gvk.Kind, "List") {
			// Skip lists since they typically yield duplicate errors
			// and do not have any specific defaulting of their own
			continue
		}

		sch := groupVersionKinds[gvk]
		gv, isPrimaryGV := groupVersions[gvk.GroupVersion()]
		if !isPrimaryGV {
			continue
		}

		var scheme *runtime.Scheme
		for _, s := range schemes {
			if s.IsVersionRegistered(gvk.GroupVersion()) {
				scheme = s
				break
			}
		}
		if scheme == nil {
			return errors.New("gvk not registered in any scheme")
		}

		example, err := scheme.New(gvk)
		if err != nil {
			return err
		}

		if err := checkDefaulting(field.NewPath(fmt.Sprintf("%v", gvk)), example, sch, gv.Components.Schemas, scheme); err != nil {
			hadError = true
			i := "    " + strings.ReplaceAll(err.Error(), "\n", "\n    ")
			fmt.Printf("- [ ] %v/%v.%v\n%v\n", gvk.Group, gvk.Version, gvk.Kind, i)
		} else {
			fmt.Printf("- [x] %v/%v.%v\n", gvk.Group, gvk.Version, gvk.Kind)
		}
	}

	if hadError {
		return errors.New("encountered violation")
	}

	return nil
}

func populateNextLevelWithEmpty(path *field.Path, obj interface{}, blacklist map[string]sets.Set[string]) bool {
	val := reflect.ValueOf(obj)
	typ := val.Type()
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
		val = val.Elem()
	}

	typKey := typ.PkgPath() + "." + typ.Name()
	blacklistForType, blacklistExists := blacklist[typKey]
	if !blacklistExists {
		blacklistForType = sets.New[string]()
		blacklist[typKey] = blacklistForType
	}

	switch typ.Kind() {
	case reflect.Struct:
		didAThing := false

		for i := 0; i < typ.NumField(); i++ {
			fld := val.Field(i)
			fldType := typ.Field(i)
			if blacklistForType.Has(fldType.Name) {
				continue
			}
			blacklistForType.Insert(fldType.Name)

			if !fld.IsZero() {
				didAThing = didAThing || populateNextLevelWithEmpty(path.Child(fldType.Name), fld.Interface(), blacklist)
				continue
			}

			switch fldType.Type.Kind() {
			case reflect.Struct:
				fld.Set(reflect.New(fld.Type()).Elem())
			case reflect.Pointer:
				fld.Set(reflect.New(fld.Type().Elem()))
				didAThing = true
			case reflect.Slice:
				fld.Set(reflect.MakeSlice(fldType.Type, 1, 1))
				didAThing = true
			case reflect.Map:
				newMap := reflect.MakeMap(fldType.Type)
				newMap.SetMapIndex(reflect.ValueOf(""), reflect.New(fldType.Type.Elem()))
				didAThing = true
			default:
				// Anything else we dont care to expand
				continue
			}
		}
		return didAThing
	case reflect.Slice:
		if val.IsZero() {
			val.Set(reflect.MakeSlice(typ, 1, 1))
			return true
		}

		return populateNextLevelWithEmpty(path.Index(0), val.Index(0).Interface(), blacklist)
	case reflect.Map:
		if val.IsZero() {
			val.SetMapIndex(reflect.ValueOf(""), reflect.New(typ.Elem()))
			return true
		}

		return populateNextLevelWithEmpty(path.Child(""), val.MapIndex(reflect.ValueOf("")).Interface(), blacklist)
	default:
		return false
	}
}

func checkDefaulting(path *field.Path, example runtime.Object, sch *spec.Schema, definitions map[string]*spec.Schema, scheme *runtime.Scheme) error {
	example = example.DeepCopyObject()

	// Map from type unique name to a stringification of field path populated
	populatedFields := map[string]sets.Set[string]{}

	for {
		if err := baseCheckDefaulting(path, example, sch, definitions, scheme); err != nil {
			return err
		}

		// Populate fields one more level. Return if nothing left to populate
		scheme.Default(example)
		if !populateNextLevelWithEmpty(field.NewPath("body"), example, populatedFields) {
			return nil
		}
	}
}

func baseCheckDefaulting(path *field.Path, example runtime.Object, sch *spec.Schema, definitions map[string]*spec.Schema, scheme *runtime.Scheme) error {
	nativeDefaulted := example.DeepCopyObject()
	schemaDefaulted := example.DeepCopyObject()

	nativeUnstructured, err := runtime.DefaultUnstructuredConverter.ToUnstructured(nativeDefaulted)
	if err != nil {
		return err
	}

	schemaUnstructured, err := runtime.DefaultUnstructuredConverter.ToUnstructured(schemaDefaulted)
	if err != nil {
		return err
	}

	Default(schemaUnstructured, sch, definitions)

	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(schemaUnstructured, schemaDefaulted); err != nil {
		return err
	}

	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(nativeUnstructured, nativeDefaulted); err != nil {
		return err
	}

	scheme.Default(nativeDefaulted)

	diff := cmp.Diff(nativeDefaulted, schemaDefaulted)
	if len(diff) != 0 {
		return fmt.Errorf("```diff\n%s\n```", diff)
	}

	return nil
}

func checkValidationForGroup(group, pkg string) []error {
	// Validation Existence Exceptions
	validationExistenceExceptions := sets.New(
		// Internal types for events.k8s.io are defined and validated in core
		// package, not events package
		"events.k8s.io",
	)

	// If any uncovered lones contains this
	blacklistedMissedValidationLines := sets.New(
		// Any call to append (assumed to be appending to error list)
		// is blacklisted as allowable to not be covered by a test
		"append(",

		// Some functions might return a singleton error list early
		"return",
	)

	if validationExistenceExceptions.Has(group) {
		return nil
	}

	validationPackagePath := filepath.Join(pkg, "validation")
	if _, err := os.Stat(validationPackagePath); err != nil {
		return []error{err}
	}

	// Validation package exists. Run go text -cover on it and determine
	// coverage level
	profilePath := filepath.Join(os.TempDir(), "check_coverage"+group+"_cover.out")
	defer os.Remove(profilePath)

	var out strings.Builder
	cmd := exec.Command("go", "test", "./", "-coverpkg", "./", "-coverprofile", profilePath)
	cmd.Dir = validationPackagePath
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return []error{fmt.Errorf("%v\n%w", out.String(), err)}
	}

	profiles, err := cover.ParseProfiles(profilePath)
	if err != nil {
		return []error{err}
	}

	var missedLines []error

	// Loop through all unhit blocks, and check whether they contain
	// any code that could possibly be an error
	for _, p := range profiles {
		// Resolve the go package filename to source
		dir, err := build.Default.Import(filepath.Dir(p.FileName), ".", build.FindOnly)

		if err != nil {
			return []error{err}
		}

		filePath := filepath.Join(dir.Dir, filepath.Base(p.FileName))
		if strings.HasPrefix(filePath, "k8s.io/kubernetes/") {
			filePath = strings.TrimPrefix(p.FileName, "k8s.io/kubernetes/")
		} else if strings.HasPrefix(filePath, "k8s.io/apiextensions-apiserver") {
			filePath = "staging/src/" + filePath
		}

		fileText, err := os.ReadFile(filePath)
		if err != nil {
			return []error{err}
		}

		boundaries := p.Boundaries(fileText)
		for i, block := range p.Blocks {
			startBoundary := boundaries[i*2]
			endBoundary := boundaries[i*2+1]

			if block.Count != 0 {
				continue
			}

			substr := string(fileText[startBoundary.Offset:endBoundary.Offset])

			// HEURISTIC: There are a few untested ValidateXUpdate
			// functions that just defer to the regular Validate function.
			// If we find a block that matches that pattern, and the function
			// is tiny enough, skip
			//!TODO: Decide if thats what we actually want to do
			pat := regexp.MustCompile(`^\s*func\s*Validate[a-zA-Z0-9]*Update.*`)
			if pat.Match([]byte(substr)) && block.NumStmt < 5 {
				continue
			}

			cwd, _ := os.Getwd()
			for blacklisted := range blacklistedMissedValidationLines {
				if strings.Contains(substr, blacklisted) {
					var printPath = filePath
					if strings.HasPrefix(filePath, cwd) {
						printPath = strings.ReplaceAll(filePath, cwd, "https://github.com/kubernetes/kubernetes/blob/2c6c4566eff972d6c1320b5f8ad795f88c822d09")
					}
					missedLines = append(missedLines, fmt.Errorf("%s#L%vC%v-L%vC%v", printPath, block.StartLine, block.StartCol, block.EndLine, block.EndCol))
					break
				}
			}
		}
	}
	return missedLines
}
