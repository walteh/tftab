// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package handlers

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	tfjson "github.com/hashicorp/terraform-json"
	"github.com/walteh/retab/internal/lsp/document"
	"github.com/walteh/retab/internal/lsp/langserver"
	"github.com/walteh/retab/internal/lsp/state"
)

func TestDefinition_basic(t *testing.T) {
	tmpDir := TempDir(t)

	ss, err := state.NewStateStore()
	if err != nil {
		t.Fatal(err)
	}

	ls := langserver.NewLangServerMock(t, NewMockSession(&MockSessionInput{
		StateStore: ss,
	}))
	stop := ls.Start(t)
	defer stop()

	ls.Call(t, &langserver.CallRequest{
		Method: "initialize",
		ReqParams: fmt.Sprintf(`{
			"capabilities": {},
			"rootUri": %q,
			"processId": 12345
	}`, tmpDir.URI)})

	ls.Notify(t, &langserver.CallRequest{
		Method:    "initialized",
		ReqParams: "{}",
	})
	ls.Call(t, &langserver.CallRequest{
		Method: "textDocument/didOpen",
		ReqParams: fmt.Sprintf(`{
		"textDocument": {
			"version": 0,
			"languageId": "retab",
			"text": `+fmt.Sprintf("%q",
			`variable "test" {
}

output "foo" {
	value = var.test
}`)+`,
			"uri": "%s/main.tf"
		}
	}`, tmpDir.URI)})

	ls.CallAndExpectResponse(t, &langserver.CallRequest{
		Method: "textDocument/definition",
		ReqParams: fmt.Sprintf(`{
			"textDocument": {
				"uri": "%s/main.tf"
			},
			"position": {
				"line": 4,
				"character": 13
			}
		}`, tmpDir.URI)}, fmt.Sprintf(`{
			"jsonrpc": "2.0",
			"id": 3,
			"result": [{
				"uri":"%s/main.tf",
				"range": {
					"start": {
						"line": 0,
						"character": 0
					},
					"end": {
						"line": 1,
						"character": 1
					}
				}
			}]
		}`, tmpDir.URI))
}

func TestDefinition_withLinkToDefLessBlock(t *testing.T) {
	tmpDir := TempDir(t)

	err := os.WriteFile(filepath.Join(tmpDir.Path(), "main.tf"), []byte("provider \"test\" {\n\n}\n"), 0o755)
	if err != nil {
		t.Fatal(err)
	}

	var testSchema tfjson.ProviderSchemas
	err = json.Unmarshal([]byte(testModuleSchemaOutput), &testSchema)
	if err != nil {
		t.Fatal(err)
	}

	ss, err := state.NewStateStore()
	if err != nil {
		t.Fatal(err)
	}

	ls := langserver.NewLangServerMock(t, NewMockSession(&MockSessionInput{
		StateStore: ss,
	}))
	stop := ls.Start(t)
	defer stop()

	ls.Call(t, &langserver.CallRequest{
		Method: "initialize",
		ReqParams: fmt.Sprintf(`{
			"capabilities": {
				"textDocument": {
					"definition": {
						"linkSupport": true
					}
				}
			},
			"rootUri": %q,
			"processId": 12345
	}`, tmpDir.URI)})

	ls.Notify(t, &langserver.CallRequest{
		Method:    "initialized",
		ReqParams: "{}",
	})
	ls.Call(t, &langserver.CallRequest{
		Method: "textDocument/didOpen",
		ReqParams: fmt.Sprintf(`{
		"textDocument": {
			"version": 0,
			"languageId": "retab",
			"text": `+fmt.Sprintf("%q",
			`resource "test_resource_2" "foo" {
    setting {
        name  = "foo"
        value = "bar"
    }
}

output "foo" {
    value = test_resource_2.foo.setting
}`)+`,
			"uri": "%s/main.tf"
		}
	}`, tmpDir.URI)})

	ls.CallAndExpectResponse(t, &langserver.CallRequest{
		Method: "textDocument/definition",
		ReqParams: fmt.Sprintf(`{
			"textDocument": {
				"uri": "%s/main.tf"
			},
			"position": {
				"line": 8,
				"character": 35
			}
		}`, tmpDir.URI)}, fmt.Sprintf(`{
			"jsonrpc": "2.0",
			"id": 3,
			"result": [
				{
					"originSelectionRange": {
						"start": {
							"line": 8,
							"character": 12
						},
						"end": {
							"line": 8,
							"character": 39
						}
					},
					"targetUri": "%s/main.tf",
					"targetRange": {
						"start": {
							"line": 1,
							"character": 4
						},
						"end": {
							"line": 4,
							"character": 5
						}
					},
					"targetSelectionRange": {
						"start": {
							"line": 1,
							"character": 4
						},
						"end": {
							"line": 4,
							"character": 5
						}
					}
				}
			]
		}`, tmpDir.URI))
}

func TestDefinition_withLinkToDefBlock(t *testing.T) {
	tmpDir := TempDir(t)

	err := os.WriteFile(filepath.Join(tmpDir.Path(), "main.tf"), []byte("provider \"test\" {\n\n}\n"), 0o755)
	if err != nil {
		t.Fatal(err)
	}

	var testSchema tfjson.ProviderSchemas
	err = json.Unmarshal([]byte(testModuleSchemaOutput), &testSchema)
	if err != nil {
		t.Fatal(err)
	}

	ss, err := state.NewStateStore()
	if err != nil {
		t.Fatal(err)
	}

	ls := langserver.NewLangServerMock(t, NewMockSession(&MockSessionInput{
		StateStore: ss,
	}))
	stop := ls.Start(t)
	defer stop()

	ls.Call(t, &langserver.CallRequest{
		Method: "initialize",
		ReqParams: fmt.Sprintf(`{
			"capabilities": {
				"textDocument": {
					"definition": {
						"linkSupport": true
					}
				}
			},
			"rootUri": %q,
			"processId": 12345
	}`, tmpDir.URI)})

	ls.Notify(t, &langserver.CallRequest{
		Method:    "initialized",
		ReqParams: "{}",
	})
	ls.Call(t, &langserver.CallRequest{
		Method: "textDocument/didOpen",
		ReqParams: fmt.Sprintf(`{
		"textDocument": {
			"version": 0,
			"languageId": "retab",
			"text": `+fmt.Sprintf("%q",
			`resource "test_resource_2" "foo" {
    setting {
        name  = "foo"
        value = "bar"
    }
}

output "foo" {
    value = test_resource_2.foo
}`)+`,
			"uri": "%s/main.tf"
		}
	}`, tmpDir.URI)})

	ls.CallAndExpectResponse(t, &langserver.CallRequest{
		Method: "textDocument/definition",
		ReqParams: fmt.Sprintf(`{
			"textDocument": {
				"uri": "%s/main.tf"
			},
			"position": {
				"line": 8,
				"character": 30
			}
		}`, tmpDir.URI)}, fmt.Sprintf(`{
			"jsonrpc": "2.0",
			"id": 3,
			"result": [
				{
					"originSelectionRange": {
						"start": {
							"line": 8,
							"character": 12
						},
						"end": {
							"line": 8,
							"character": 31
						}
					},
					"targetUri": "%s/main.tf",
					"targetRange": {
						"start": {
							"line": 0,
							"character": 0
						},
						"end": {
							"line": 5,
							"character": 1
						}
					},
					"targetSelectionRange": {
						"start": {
							"line": 0,
							"character": 0
						},
						"end": {
							"line": 0,
							"character": 32
						}
					}
				}
			]
		}`, tmpDir.URI))
}

func TestDefinition_moduleInputToVariable(t *testing.T) {
	modPath, err := filepath.Abs(filepath.Join("testdata", "single-submodule"))
	if err != nil {
		t.Fatal(err)
	}
	modHandle := document.DirHandleFromPath(modPath)

	ss, err := state.NewStateStore()
	if err != nil {
		t.Fatal(err)
	}

	ls := langserver.NewLangServerMock(t, NewMockSession(&MockSessionInput{
		StateStore: ss,
	}))
	stop := ls.Start(t)
	defer stop()

	ls.Call(t, &langserver.CallRequest{
		Method: "initialize",
		ReqParams: fmt.Sprintf(`{
			"capabilities": {},
			"rootUri": %q,
			"processId": 12345
	}`, modHandle.URI)})

	ls.Notify(t, &langserver.CallRequest{
		Method:    "initialized",
		ReqParams: "{}",
	})
	ls.Call(t, &langserver.CallRequest{
		Method: "textDocument/didOpen",
		ReqParams: fmt.Sprintf(`{
		"textDocument": {
			"version": 0,
			"languageId": "retab",
			"text": `+fmt.Sprintf("%q",
			`module "gorilla-app" {
	source           = "./application"
	environment_name = "prod"
	app_prefix       = "protect-gorillas"
	instances        = 5
}
`)+`,
			"uri": "%s/main.tf"
		}
	}`, modHandle.URI)})

	ls.CallAndExpectResponse(t, &langserver.CallRequest{
		Method: "textDocument/definition",
		ReqParams: fmt.Sprintf(`{
			"textDocument": {
				"uri": "%s/main.tf"
			},
			"position": {
				"line": 2,
				"character": 6
			}
		}`, modHandle.URI)}, fmt.Sprintf(`{
			"jsonrpc": "2.0",
			"id": 3,
			"result": [
				{
						"uri": "%s/application/main.tf",
						"range": {
								"start": {
										"line": 0,
										"character": 0
								},
								"end": {
										"line": 2,
										"character": 1
								}
						}
				}
			]
		}`, modHandle.URI))
}

func TestDeclaration_basic(t *testing.T) {
	tmpDir := TempDir(t)

	ss, err := state.NewStateStore()
	if err != nil {
		t.Fatal(err)
	}

	ls := langserver.NewLangServerMock(t, NewMockSession(&MockSessionInput{
		StateStore: ss,
	}))
	stop := ls.Start(t)
	defer stop()

	ls.Call(t, &langserver.CallRequest{
		Method: "initialize",
		ReqParams: fmt.Sprintf(`{
			"capabilities": {},
			"rootUri": %q,
			"processId": 12345
	}`, tmpDir.URI)})

	ls.Notify(t, &langserver.CallRequest{
		Method:    "initialized",
		ReqParams: "{}",
	})
	ls.Call(t, &langserver.CallRequest{
		Method: "textDocument/didOpen",
		ReqParams: fmt.Sprintf(`{
		"textDocument": {
			"version": 0,
			"languageId": "retab",
			"text": `+fmt.Sprintf("%q",
			`variable "test" {
}

output "foo" {
	value = var.test
}`)+`,
			"uri": "%s/main.tf"
		}
	}`, tmpDir.URI)})

	ls.CallAndExpectResponse(t, &langserver.CallRequest{
		Method: "textDocument/declaration",
		ReqParams: fmt.Sprintf(`{
			"textDocument": {
				"uri": "%s/main.tf"
			},
			"position": {
				"line": 4,
				"character": 13
			}
		}`, tmpDir.URI)}, fmt.Sprintf(`{
			"jsonrpc": "2.0",
			"id": 3,
			"result": [{
				"uri":"%s/main.tf",
				"range": {
					"start": {
						"line": 0,
						"character": 0
					},
					"end": {
						"line": 1,
						"character": 1
					}
				}
			}]
		}`, tmpDir.URI))
}

func TestDeclaration_withLinkSupport(t *testing.T) {
	tmpDir := TempDir(t)

	err := os.WriteFile(filepath.Join(tmpDir.Path(), "main.tf"), []byte("provider \"test\" {\n\n}\n"), 0o755)
	if err != nil {
		t.Fatal(err)
	}

	var testSchema tfjson.ProviderSchemas
	err = json.Unmarshal([]byte(testModuleSchemaOutput), &testSchema)
	if err != nil {
		t.Fatal(err)
	}

	ss, err := state.NewStateStore()
	if err != nil {
		t.Fatal(err)
	}

	ls := langserver.NewLangServerMock(t, NewMockSession(&MockSessionInput{
		StateStore: ss,
	}))
	stop := ls.Start(t)
	defer stop()

	ls.Call(t, &langserver.CallRequest{
		Method: "initialize",
		ReqParams: fmt.Sprintf(`{
			"capabilities": {
				"textDocument": {
					"declaration": {
						"linkSupport": true
					}
				}
			},
			"rootUri": %q,
			"processId": 12345
	}`, tmpDir.URI)})

	ls.Notify(t, &langserver.CallRequest{
		Method:    "initialized",
		ReqParams: "{}",
	})
	ls.Call(t, &langserver.CallRequest{
		Method: "textDocument/didOpen",
		ReqParams: fmt.Sprintf(`{
		"textDocument": {
			"version": 0,
			"languageId": "retab",
			"text": `+fmt.Sprintf("%q",
			`resource "test_resource_2" "foo" {
    setting {
        name  = "foo"
        value = "bar"
    }
}

output "foo" {
    value = test_resource_2.foo.setting
}`)+`,
			"uri": "%s/main.tf"
		}
	}`, tmpDir.URI)})

	ls.CallAndExpectResponse(t, &langserver.CallRequest{
		Method: "textDocument/declaration",
		ReqParams: fmt.Sprintf(`{
			"textDocument": {
				"uri": "%s/main.tf"
			},
			"position": {
				"line": 8,
				"character": 35
			}
		}`, tmpDir.URI)}, fmt.Sprintf(`{
			"jsonrpc": "2.0",
			"id": 3,
			"result": [
				{
					"originSelectionRange": {
						"start": {
							"line": 8,
							"character": 12
						},
						"end": {
							"line": 8,
							"character": 39
						}
					},
					"targetUri": "%s/main.tf",
					"targetRange": {
						"start": {
							"line": 1,
							"character": 4
						},
						"end": {
							"line": 4,
							"character": 5
						}
					},
					"targetSelectionRange": {
						"start": {
							"line": 1,
							"character": 4
						},
						"end": {
							"line": 4,
							"character": 5
						}
					}
				}
			]
		}`, tmpDir.URI))
}
