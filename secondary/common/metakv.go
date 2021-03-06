// @author Couchbase <info@couchbase.com>
// @copyright 2015 Couchbase, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package common

import (
	"encoding/json"
	"fmt"
	"github.com/couchbase/cbauth/metakv"
	"github.com/couchbase/indexing/secondary/logging"
	"strconv"
	"strings"
)

const (
	METAKV_SIZE_LIMIT = 2048
)

func MetakvGet(path string, v interface{}) (bool, error) {
	raw, _, err := metakv.Get(path)
	if err != nil {
		logging.Fatalf("MetakvGet: Failed to fetch %s from metakv: %s", path, err.Error())
	}

	if raw == nil {
		return false, err
	}

	err = json.Unmarshal(raw, v)
	if err != nil {
		logging.Fatalf("MetakvGet: Failed unmarshalling value for %s: %s\n%s",
			path, err.Error(), string(raw))
		return false, err
	}

	return true, nil
}

func MetakvSet(path string, v interface{}) error {
	raw, err := json.Marshal(v)
	if err != nil {
		logging.Fatalf("MetakvSet: Failed to marshal value for %s: %s\n%v",
			path, err.Error(), v)
		return err
	}

	err = metakv.Set(path, raw, nil)
	if err != nil {
		logging.Fatalf("MetakvSet Failed to set %s: %s", path, err.Error())
	}
	return err
}

func MetakvDel(path string) error {

	err := metakv.Delete(path, nil)
	if err != nil {
		logging.Fatalf("MetakvDel: Failed to delete %s: %s", path, err.Error())
	}
	return err
}

func MetakvRecurciveDel(dirpath string) error {

	err := metakv.RecursiveDelete(dirpath)
	if err != nil {
		logging.Fatalf("MetakvRecurciveDel: Failed to delete %s: %s", dirpath, err.Error())
	}
	return err
}

func MetakvList(dirpath string) ([]string, error) {

	if len(dirpath) == 0 {
		return nil, fmt.Errorf("Empty metakv path")
	}

	if string(dirpath[len(dirpath)-1]) != "/" {
		dirpath = fmt.Sprintf("%v/", dirpath)
	}

	entries, err := metakv.ListAllChildren(dirpath)
	if err != nil {
		return nil, err
	}

	if len(entries) == 0 {
		return nil, nil
	}

	var result []string
	for _, entry := range entries {
		result = append(result, entry.Path)
	}

	return result, nil
}

func MetakvBigValueSet(path string, v interface{}) error {

	if len(path) == 0 {
		return fmt.Errorf("Empty metakv path")
	}

	if string(path[len(path)-1]) == "/" {
		path = path[:len(path)-1]
	}

	buf, err := json.Marshal(v)
	if err != nil {
		return err
	}

	len := len(buf)
	count := 0

	for len > 0 {
		path2 := fmt.Sprintf("%v/%v", path, count)

		size := len
		if size > METAKV_SIZE_LIMIT {
			size = METAKV_SIZE_LIMIT
		}

		if err = metakv.Set(path2, buf[:size], nil); err != nil {
			MetakvBigValueDel(path)
			return err
		}

		count++
		len -= size
		if len > 0 {
			buf = buf[size:]
		}
	}

	return nil
}

func MetakvBigValueDel(path string) error {

	if len(path) == 0 {
		return fmt.Errorf("Empty metakv path")
	}

	if string(path[len(path)-1]) != "/" {
		path = fmt.Sprintf("%v/", path)
	}

	return MetakvRecurciveDel(path)
}

func MetakvBigValueGet(path string, value interface{}) (bool, error) {

	if len(path) == 0 {
		return false, fmt.Errorf("Empty metakv path")
	}

	if string(path[len(path)-1]) != "/" {
		path = fmt.Sprintf("%v/", path)
	}

	entries, err := metakv.ListAllChildren(path)
	if err != nil {
		return false, err
	}

	if len(entries) == 0 {
		// value not exist
		return false, nil
	}

	raws := make([][]byte, len(entries))
	for _, entry := range entries {
		loc := strings.LastIndex(entry.Path, "/")
		if loc == -1 || loc == len(entry.Path)-1 {
			return false, fmt.Errorf("Unable to identify index for %v", entry.Path)
		}
		index, err := strconv.Atoi(entry.Path[loc+1:])
		if err != nil {
			return false, err
		}
		if index >= len(entries) {
			//The value is not fully formed.
			// Do not return error, but says value not exist.
			return false, nil
		}
		raws[index] = entry.Value
	}

	var buf []byte
	for _, raw := range raws {
		//The value is not fully formed.
		// Do not return error, but says value not exist.
		if raw == nil {
			return false, nil
		}
		buf = append(buf, raw...)
	}

	if err := json.Unmarshal(buf, value); err != nil {
		return false, err
	}

	return true, nil
}

// prefix must end with '/'
func MetakvBigValueList(dirpath string) ([]string, error) {

	if len(dirpath) == 0 {
		return nil, fmt.Errorf("Empty metakv path")
	}

	if string(dirpath[len(dirpath)-1]) != "/" {
		dirpath = fmt.Sprintf("%v/", dirpath)
	}

	entries, err := metakv.ListAllChildren(dirpath)
	if err != nil {
		return nil, err
	}

	if len(entries) == 0 {
		return nil, nil
	}

	paths := make(map[string]bool)
	for _, entry := range entries {
		path := entry.Path

		loc := strings.LastIndex(path, "/")
		if loc == -1 || loc == len(path)-1 {
			continue
		}
		path = path[:loc]

		if len(path) <= len(dirpath) {
			continue
		}

		paths[path] = true
	}

	var result []string
	for path, _ := range paths {
		result = append(result, path)
	}

	return result, nil
}
