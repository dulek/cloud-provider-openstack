/*
Copyright 2018 The Kubernetes Authors.

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

package sharebackends

import (
	"fmt"
	"k8s.io/cloud-provider-openstack/pkg/csi/manila/manilaclient"
	"strings"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/sharedfilesystems/v2/shares"
	"k8s.io/api/core/v1"
	clientset "k8s.io/client-go/kubernetes"
)

// Splits ExportLocation path "addr1:port,addr2:port,...:/location" into its address
// and location parts. The last occurrence of ':' is considered as the delimiter
// between those two parts.
func splitExportLocation(loc *shares.ExportLocation) (address, location string, err error) {
	delimPos := strings.LastIndexByte(loc.Path, ':')
	if delimPos <= 0 {
		err = fmt.Errorf("failed to parse address and location from export location '%s'", loc.Path)
		return
	}

	address = loc.Path[:delimPos]
	location = loc.Path[delimPos+1:]

	return
}

func createSecret(secretRef *v1.SecretReference, cs clientset.Interface, data map[string][]byte) error {
	sec := v1.Secret{Data: data}
	sec.Name = secretRef.Name

	if _, err := cs.CoreV1().Secrets(secretRef.Namespace).Create(&sec); err != nil {
		return err
	}

	return nil
}

func deleteSecret(secretRef *v1.SecretReference, cs clientset.Interface) error {
	return cs.CoreV1().Secrets(secretRef.Namespace).Delete(secretRef.Name, nil)
}

func getAccess(shareID string, c manilaclient.Interface, accessID string) (*shares.AccessRight, error) {
	accessRights, err := c.GetAccessRights(shareID)
	if err != nil {
		return nil, err
	}

	for i := range accessRights {
		if accessRights[i].ID == accessID {
			return &accessRights[i], nil
		}
	}

	return nil, fmt.Errorf("access rule %s for share %s not found", accessID, shareID)
}

// Grants access to Ceph share. Since Ceph share keys are generated by Ceph backend,
// they're not contained in the response from shares.GrantAccess(), but have to be
// queried for separately by subsequent ListAccessRights call(s)
func grantAccessCephx(args *GrantAccessArgs) (*shares.AccessRight, error) {
	accessOpts := shares.GrantAccessOpts{
		AccessType:  "cephx",
		AccessTo:    args.Share.Name,
		AccessLevel: "rw",
	}

	if _, err := args.Client.GrantAccess(args.Share.ID, accessOpts); err != nil {
		return nil, err
	}

	var accessRight shares.AccessRight

	err := gophercloud.WaitFor(120, func() (bool, error) {
		accessRights, err := args.Client.GetAccessRights(args.Share.ID)
		if err != nil {
			return false, err
		}

		if len(accessRights) > 1 {
			return false, fmt.Errorf("unexpected number of access rules: got %d, expected 1", len(accessRights))
		} else if len(accessRights) == 0 {
			return false, nil
		}

		if accessRights[0].AccessKey != "" {
			accessRight = accessRights[0]
			return true, nil
		}

		return false, nil
	})

	return &accessRight, err
}

func getOrCreateCephxAccess(args *GrantAccessArgs) (*shares.AccessRight, error) {
	var (
		accessRight *shares.AccessRight
		err         error
	)

	if args.Options.OSShareAccessID != "" {
		accessRight, err = getAccess(args.Share.ID, args.Client, args.Options.OSShareAccessID)
	} else {
		accessRight, err = grantAccessCephx(args)
	}

	if err != nil {
		return nil, err
	}

	if accessRight.AccessType != "cephx" {
		return nil, fmt.Errorf("wrong type for access rule %s in share %s: expected cephx, got %s",
			accessRight.ID, args.Share.ID, accessRight.AccessType)
	}

	if accessRight.AccessKey == "" || accessRight.AccessTo == "" {
		return nil, fmt.Errorf("missing AccessKey or AccessTo for access rule %s in share %s", accessRight.ID, args.Share.ID)
	}

	return accessRight, err
}
