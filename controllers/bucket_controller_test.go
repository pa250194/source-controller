/*
Copyright 2021 The Flux authors

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

package controllers

import (
	"context"
	"crypto/md5"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/fluxcd/pkg/apis/meta"
	"github.com/fluxcd/pkg/runtime/conditions"

	sourcev1 "github.com/fluxcd/source-controller/api/v1beta1"
)

func TestBucketReconciler_Reconcile(t *testing.T) {
	g := NewWithT(t)

	s3Server := newS3Server("test-bucket")
	s3Server.Objects = []*s3MockObject{
		{
			Key:          "test.txt",
			Content:      []byte("test"),
			ContentType:  "text/plain",
			LastModified: time.Now(),
		},
	}
	s3Server.Start()
	defer s3Server.Stop()

	g.Expect(s3Server.HTTPAddress()).ToNot(BeEmpty())
	u, err := url.Parse(s3Server.HTTPAddress())
	g.Expect(err).NotTo(HaveOccurred())

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "bucket-reconcile-",
			Namespace:    "default",
		},
		Data: map[string][]byte{
			"accesskey": []byte("key"),
			"secretkey": []byte("secret"),
		},
	}
	g.Expect(env.Create(ctx, secret)).To(Succeed())
	defer env.Delete(ctx, secret)

	obj := &sourcev1.Bucket{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "bucket-reconcile-",
			Namespace:    "default",
		},
		Spec: sourcev1.BucketSpec{
			Provider:   "generic",
			BucketName: s3Server.BucketName,
			Endpoint:   u.Host,
			Insecure:   true,
			Interval:   metav1.Duration{Duration: interval},
			Timeout:    &metav1.Duration{Duration: timeout},
			SecretRef: &meta.LocalObjectReference{
				Name: secret.Name,
			},
		},
	}
	g.Expect(env.Create(ctx, obj)).To(Succeed())

	key := client.ObjectKey{Name: obj.Name, Namespace: obj.Namespace}

	// Wait for finalizer to be set
	g.Eventually(func() bool {
		if err := env.Get(ctx, key, obj); err != nil {
			return false
		}
		return len(obj.Finalizers) > 0
	}, timeout).Should(BeTrue())

	// Wait for Bucket to be Ready
	g.Eventually(func() bool {
		if err := env.Get(ctx, key, obj); err != nil {
			return false
		}

		if !conditions.Has(obj, sourcev1.ArtifactAvailableCondition) ||
			!conditions.Has(obj, sourcev1.SourceAvailableCondition) ||
			!conditions.Has(obj, meta.ReadyCondition) ||
			obj.Status.Artifact == nil {
			return false
		}

		readyCondition := conditions.Get(obj, meta.ReadyCondition)

		return readyCondition.Status == metav1.ConditionTrue &&
			obj.Generation == readyCondition.ObservedGeneration
	}, timeout).Should(BeTrue())

	g.Expect(env.Delete(ctx, obj)).To(Succeed())

	// Wait for Bucket to be deleted
	g.Eventually(func() bool {
		if err := env.Get(ctx, key, obj); err != nil {
			return apierrors.IsNotFound(err)
		}
		return false
	}, timeout).Should(BeTrue())
}

func TestBucketReconciler_reconcileStorage(t *testing.T) {
	tests := []struct {
		name             string
		beforeFunc       func(obj *sourcev1.Bucket, storage *Storage) error
		want             ctrl.Result
		wantErr          bool
		assertArtifact   *sourcev1.Artifact
		assertConditions []metav1.Condition
		assertPaths      []string
	}{
		{
			name: "garbage collects",
			beforeFunc: func(obj *sourcev1.Bucket, storage *Storage) error {
				revisions := []string{"a", "b", "c"}
				for n := range revisions {
					v := revisions[n]
					obj.Status.Artifact = &sourcev1.Artifact{
						Path:     fmt.Sprintf("/reconcile-storage/%s.txt", v),
						Revision: v,
					}
					if err := storage.MkdirAll(*obj.Status.Artifact); err != nil {
						return err
					}
					if err := storage.AtomicWriteFile(obj.Status.Artifact, strings.NewReader(v), 0644); err != nil {
						return err
					}
				}
				storage.SetArtifactURL(obj.Status.Artifact)
				return nil
			},
			assertArtifact: &sourcev1.Artifact{
				Path:     "/reconcile-storage/c.txt",
				Revision: "c",
				Checksum: "84a516841ba77a5b4648de2cd0dfcb30ea46dbb4",
				URL:      storage.Hostname + "/reconcile-storage/c.txt",
			},
			assertPaths: []string{
				"/reconcile-storage/c.txt",
				"!/reconcile-storage/b.txt",
				"!/reconcile-storage/a.txt",
			},
		},
		{
			name: "notices missing artifact in storage",
			beforeFunc: func(obj *sourcev1.Bucket, storage *Storage) error {
				obj.Status.Artifact = &sourcev1.Artifact{
					Path:     fmt.Sprintf("/reconcile-storage/invalid.txt"),
					Revision: "d",
				}
				storage.SetArtifactURL(obj.Status.Artifact)
				return nil
			},
			want: ctrl.Result{Requeue: true},
			assertPaths: []string{
				"!/reconcile-storage/invalid.txt",
			},
			assertConditions: []metav1.Condition{
				*conditions.FalseCondition(sourcev1.ArtifactAvailableCondition, "NoArtifactFound", "No artifact for resource in storage"),
			},
		},
		{
			name: "updates hostname on diff from current",
			beforeFunc: func(obj *sourcev1.Bucket, storage *Storage) error {
				obj.Status.Artifact = &sourcev1.Artifact{
					Path:     fmt.Sprintf("/reconcile-storage/hostname.txt"),
					Revision: "f",
					Checksum: "971c419dd609331343dee105fffd0f4608dc0bf2",
					URL:      "http://outdated.com/reconcile-storage/hostname.txt",
				}
				if err := storage.MkdirAll(*obj.Status.Artifact); err != nil {
					return err
				}
				if err := storage.AtomicWriteFile(obj.Status.Artifact, strings.NewReader("file"), 0644); err != nil {
					return err
				}
				return nil
			},
			assertPaths: []string{
				"/reconcile-storage/hostname.txt",
			},
			assertArtifact: &sourcev1.Artifact{
				Path:     "/reconcile-storage/hostname.txt",
				Revision: "f",
				Checksum: "971c419dd609331343dee105fffd0f4608dc0bf2",
				URL:      storage.Hostname + "/reconcile-storage/hostname.txt",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			r := &BucketReconciler{
				Storage: storage,
			}

			obj := &sourcev1.Bucket{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "test-",
				},
			}
			if tt.beforeFunc != nil {
				g.Expect(tt.beforeFunc(obj, storage)).To(Succeed())
			}

			got, err := r.reconcileStorage(context.TODO(), obj)
			g.Expect(err != nil).To(Equal(tt.wantErr))
			g.Expect(got).To(Equal(tt.want))

			g.Expect(obj.Status.Artifact).To(MatchArtifact(tt.assertArtifact))
			if tt.assertArtifact != nil && tt.assertArtifact.URL != "" {
				g.Expect(obj.Status.Artifact.URL).To(Equal(tt.assertArtifact.URL))
			}
			g.Expect(obj.Status.Conditions).To(conditions.MatchConditions(tt.assertConditions))

			for _, p := range tt.assertPaths {
				absoluteP := filepath.Join(storage.BasePath, p)
				if !strings.HasPrefix(p, "!") {
					g.Expect(absoluteP).To(BeAnExistingFile())
					continue
				}
				g.Expect(absoluteP).NotTo(BeAnExistingFile())
			}
		})
	}
}

func TestBucketReconciler_reconcileSource(t *testing.T) {
	tests := []struct {
		name             string
		bucketName       string
		bucketObjects    []*s3MockObject
		middleware       http.Handler
		secret           *corev1.Secret
		beforeFunc       func(obj *sourcev1.Bucket)
		want             ctrl.Result
		wantErr          bool
		assertArtifact   sourcev1.Artifact
		assertConditions []metav1.Condition
	}{
		{
			name:       "reconciles source",
			bucketName: "dummy",
			bucketObjects: []*s3MockObject{
				{
					Key:          "test.txt",
					Content:      []byte("test"),
					ContentType:  "text/plain",
					LastModified: time.Now(),
				},
			},
			assertArtifact: sourcev1.Artifact{
				Path:     "bucket/test-bucket/8f6e217935490b6283d2f257576e2f8674d70963.tar.gz",
				Revision: "8f6e217935490b6283d2f257576e2f8674d70963",
			},
			assertConditions: []metav1.Condition{
				*conditions.TrueCondition(sourcev1.SourceAvailableCondition, sourcev1.BucketOperationSucceedReason, "Downloaded 1 objects from bucket"),
			},
		},
		// TODO(hidde): middleware for mock server
		//{
		//	name: "authenticates using secretRef",
		//	bucketName: "dummy",
		//},
		{
			name:       "observes non-existing secretRef",
			bucketName: "dummy",
			beforeFunc: func(obj *sourcev1.Bucket) {
				obj.Spec.SecretRef = &meta.LocalObjectReference{
					Name: "dummy",
				}
			},
			assertConditions: []metav1.Condition{
				*conditions.FalseCondition(sourcev1.SourceAvailableCondition, sourcev1.AuthenticationFailedReason, "Failed to get secret '/dummy': secrets \"dummy\" not found"),
			},
		},
		{
			name:       "observes invalid secretRef",
			bucketName: "dummy",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name: "dummy",
				},
			},
			beforeFunc: func(obj *sourcev1.Bucket) {
				obj.Spec.SecretRef = &meta.LocalObjectReference{
					Name: "dummy",
				}
			},
			assertConditions: []metav1.Condition{
				*conditions.FalseCondition(sourcev1.SourceAvailableCondition, sourcev1.BucketOperationFailedReason, "Failed to construct S3 client: invalid \"dummy\" secret data: required fields"),
			},
		},
		{
			name:       "observes non-existing bucket name",
			bucketName: "dummy",
			beforeFunc: func(obj *sourcev1.Bucket) {
				obj.Spec.BucketName = "invalid"
			},
			assertConditions: []metav1.Condition{
				*conditions.FalseCondition(sourcev1.SourceAvailableCondition, sourcev1.BucketOperationFailedReason, "Bucket \"invalid\" does not exist"),
			},
		},
		{
			name: "transient bucket name API failure",
			beforeFunc: func(obj *sourcev1.Bucket) {
				obj.Spec.Endpoint = "transient.example.com"
				obj.Spec.BucketName = "unavailable"
			},
			wantErr: true,
			assertConditions: []metav1.Condition{
				*conditions.FalseCondition(sourcev1.SourceAvailableCondition, sourcev1.BucketOperationFailedReason, "Failed to verify existence of bucket \"unavailable\""),
			},
		},
		{
			// TODO(hidde): test the lesser happy paths
			name:       ".sourceignore",
			bucketName: "dummy",
			bucketObjects: []*s3MockObject{
				{
					Key:          ".sourceignore",
					Content:      []byte("ignored/file.txt"),
					ContentType:  "text/plain",
					LastModified: time.Now(),
				},
				{
					Key:          "ignored/file.txt",
					Content:      []byte("ignored/file.txt"),
					ContentType:  "text/plain",
					LastModified: time.Now(),
				},
				{
					Key:          "included/file.txt",
					Content:      []byte("included/file.txt"),
					ContentType:  "text/plain",
					LastModified: time.Now(),
				},
			},
			assertArtifact: sourcev1.Artifact{
				Path:     "bucket/test-bucket/36d3a0fb2a71d0026e66af1495ff0879ad5ff54e.tar.gz",
				Revision: "36d3a0fb2a71d0026e66af1495ff0879ad5ff54e",
			},
			assertConditions: []metav1.Condition{
				*conditions.TrueCondition(sourcev1.SourceAvailableCondition, sourcev1.BucketOperationSucceedReason, "Downloaded 1 objects from bucket"),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			builder := fakeclient.NewClientBuilder().WithScheme(env.Scheme())
			if tt.secret != nil {
				builder.WithObjects(tt.secret)
			}
			r := &BucketReconciler{
				Client:  builder.Build(),
				Storage: storage,
			}
			tmpDir, err := ioutil.TempDir("", "reconcile-bucket-source-")
			g.Expect(err).ToNot(HaveOccurred())
			defer os.RemoveAll(tmpDir)

			obj := &sourcev1.Bucket{
				TypeMeta: metav1.TypeMeta{
					Kind: sourcev1.BucketKind,
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-bucket",
				},
				Spec: sourcev1.BucketSpec{
					Timeout: &metav1.Duration{Duration: timeout},
				},
			}

			var server *s3MockServer
			if tt.bucketName != "" {
				server = newS3Server(tt.bucketName)
				server.Objects = tt.bucketObjects
				server.Start()
				defer server.Stop()

				g.Expect(server.HTTPAddress()).ToNot(BeEmpty())
				u, err := url.Parse(server.HTTPAddress())
				g.Expect(err).NotTo(HaveOccurred())

				obj.Spec.BucketName = tt.bucketName
				obj.Spec.Endpoint = u.Host
				// TODO(hidde): also test TLS
				obj.Spec.Insecure = true
			}
			if tt.beforeFunc != nil {
				tt.beforeFunc(obj)
			}

			artifact := &sourcev1.Artifact{}
			got, err := r.reconcileSource(context.TODO(), obj, artifact, tmpDir)
			g.Expect(err != nil).To(Equal(tt.wantErr))
			g.Expect(got).To(Equal(tt.want))

			g.Expect(artifact).To(MatchArtifact(tt.assertArtifact.DeepCopy()))
			g.Expect(obj.Status.Conditions).To(conditions.MatchConditions(tt.assertConditions))
		})
	}
}

func TestBucketReconciler_reconcileArtifact(t *testing.T) {
	tests := []struct {
		name             string
		artifact         sourcev1.Artifact
		beforeFunc       func(obj *sourcev1.Bucket, artifact sourcev1.Artifact, dir string)
		want             ctrl.Result
		wantErr          bool
		assertConditions []metav1.Condition
	}{
		{
			name: "artifact revision up-to-date",
			artifact: sourcev1.Artifact{
				Revision: "existing",
			},
			beforeFunc: func(obj *sourcev1.Bucket, artifact sourcev1.Artifact, dir string) {
				obj.Status.Artifact = &artifact
			},
			assertConditions: []metav1.Condition{
				*conditions.TrueCondition(sourcev1.ArtifactAvailableCondition, meta.SucceededReason, "Compressed source to artifact with revision 'existing'"),
			},
		},
		{
			name: "dir path deleted",
			beforeFunc: func(obj *sourcev1.Bucket, artifact sourcev1.Artifact, dir string) {
				_ = os.RemoveAll(dir)
			},
			wantErr: true,
			assertConditions: []metav1.Condition{
				*conditions.FalseCondition(sourcev1.ArtifactAvailableCondition, sourcev1.StorageOperationFailedReason, "Failed to stat source path"),
			},
		},
		//{
		//	name: "dir path empty",
		//},
		//{
		//	name: "success",
		//	artifact: sourcev1.Artifact{
		//		Revision: "existing",
		//	},
		//	beforeFunc: func(obj *sourcev1.Bucket, artifact sourcev1.Artifact, dir string) {
		//		obj.Status.Artifact = &artifact
		//	},
		//	assertConditions: []metav1.Condition{
		//		*conditions.TrueCondition(sourcev1.ArtifactAvailableCondition, meta.SucceededReason, "Compressed source to artifact with revision 'existing'"),
		//	},
		//},
		//{
		//	name: "symlink",
		//},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			tmpDir, err := ioutil.TempDir("", "reconcile-bucket-artifact-")
			g.Expect(err).ToNot(HaveOccurred())
			defer os.RemoveAll(tmpDir)

			obj := &sourcev1.Bucket{
				TypeMeta: metav1.TypeMeta{
					Kind: sourcev1.BucketKind,
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-bucket",
				},
				Spec: sourcev1.BucketSpec{
					Timeout: &metav1.Duration{Duration: timeout},
				},
			}

			if tt.beforeFunc != nil {
				tt.beforeFunc(obj, tt.artifact, tmpDir)
			}

			r := &BucketReconciler{
				Storage: storage,
			}

			got, err := r.reconcileArtifact(logr.NewContext(ctx, log.NullLogger{}), obj, tt.artifact, tmpDir)
			g.Expect(err != nil).To(Equal(tt.wantErr))
			g.Expect(got).To(Equal(tt.want))

			//g.Expect(artifact).To(MatchArtifact(tt.assertArtifact.DeepCopy()))
			g.Expect(obj.Status.Conditions).To(conditions.MatchConditions(tt.assertConditions))
		})
	}
}

func TestBucketReconciler_checksum(t *testing.T) {
	tests := []struct {
		name       string
		beforeFunc func(root string)
		want       string
		wantErr    bool
	}{
		{
			name: "empty root",
			want: "da39a3ee5e6b4b0d3255bfef95601890afd80709",
		},
		{
			name: "with file",
			beforeFunc: func(root string) {
				mockFile(root, "a/b/c.txt", "a dummy string")
			},
			want: "309a5e6e96b4a7eea0d1cfaabf1be8ec1c063fa0",
		},
		{
			name: "with file in different path",
			beforeFunc: func(root string) {
				mockFile(root, "a/b.txt", "a dummy string")
			},
			want: "e28c62b5cc488849950c4355dddc5523712616d4",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, err := ioutil.TempDir("", "bucket-checksum-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(root)
			if tt.beforeFunc != nil {
				tt.beforeFunc(root)
			}
			got, err := (&BucketReconciler{}).checksum(root)
			if (err != nil) != tt.wantErr {
				t.Errorf("checksum() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("checksum() got = %v, want %v", got, tt.want)
			}
		})
	}
}

// helpers

func mockFile(root, path, content string) error {
	filePath := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(filePath), os.ModePerm); err != nil {
		panic(err)
	}
	if err := ioutil.WriteFile(filePath, []byte(content), 0644); err != nil {
		panic(err)
	}
	return nil
}

type s3MockObject struct {
	Key          string
	LastModified time.Time
	ContentType  string
	Content      []byte
}

type s3MockServer struct {
	srv *httptest.Server
	mux *http.ServeMux

	BucketName string
	Objects    []*s3MockObject
}

func newS3Server(bucketName string) *s3MockServer {
	s := &s3MockServer{BucketName: bucketName}
	s.mux = http.NewServeMux()
	s.mux.Handle(fmt.Sprintf("/%s/", s.BucketName), http.HandlerFunc(s.handler))

	s.srv = httptest.NewUnstartedServer(s.mux)

	return s
}

func (s *s3MockServer) Start() {
	s.srv.Start()
}

func (s *s3MockServer) Stop() {
	s.srv.Close()
}

func (s *s3MockServer) HTTPAddress() string {
	return s.srv.URL
}

func (s *s3MockServer) handler(w http.ResponseWriter, r *http.Request) {
	key := path.Base(r.URL.Path)

	switch key {
	case s.BucketName:
		w.Header().Add("Content-Type", "application/xml")

		if r.Method == http.MethodHead {
			return
		}

		q := r.URL.Query()

		if q["location"] != nil {
			fmt.Fprint(w, `
<?xml version="1.0" encoding="UTF-8"?>
<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/">Europe</LocationConstraint>
			`)
			return
		}

		contents := ""
		for _, o := range s.Objects {
			etag := md5.Sum(o.Content)
			contents += fmt.Sprintf(`
		<Contents>
			<Key>%s</Key>
			<LastModified>%s</LastModified>
			<Size>%d</Size>
			<ETag>&quot;%b&quot;</ETag>
			<StorageClass>STANDARD</StorageClass>
		</Contents>`, o.Key, o.LastModified.UTC().Format(time.RFC3339), len(o.Content), etag)
		}

		fmt.Fprintf(w, `
<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
	<Name>%s</Name>
	<Prefix/>
	<Marker/>
	<KeyCount>%d</KeyCount>
	<MaxKeys>1000</MaxKeys>
	<IsTruncated>false</IsTruncated>
	%s
</ListBucketResult>
		`, s.BucketName, len(s.Objects), contents)
	default:
		key, err := filepath.Rel("/"+s.BucketName, r.URL.Path)
		if err != nil {
			w.WriteHeader(500)
			return
		}

		var found *s3MockObject
		for _, o := range s.Objects {
			if key == o.Key {
				found = o
			}
		}
		if found == nil {
			w.WriteHeader(404)
			return
		}

		etag := md5.Sum(found.Content)
		lastModified := strings.Replace(found.LastModified.UTC().Format(time.RFC1123), "UTC", "GMT", 1)

		w.Header().Add("Content-Type", found.ContentType)
		w.Header().Add("Last-Modified", lastModified)
		w.Header().Add("ETag", fmt.Sprintf("\"%b\"", etag))
		w.Header().Add("Content-Length", fmt.Sprintf("%d", len(found.Content)))

		if r.Method == http.MethodHead {
			return
		}

		w.Write(found.Content)
	}
}