# AWS mock-service URL relocation review

Status: approved by the user and applied on 5 October 2026 as revision 176. A fresh plan matched the reviewed hash and contained only the 39 approved upstream_url changes. A migrated API successfully proxied to the cloud mock service; gateway readiness and Supabase health passed.

The deployed console is https://console.13.210.150.88.sslip.io.

Replace only the host portion `http://localhost:7070` or `http://127.0.0.1:7070` with `http://mockupstream:7070` for the 39 APIs below. Preserve URL path/query, API IDs, names, authentication, permissions, plans, subscriptions, visibility and enabled/draft states. Seven other local-only upstreams remain unchanged.

Validation: valid; 39 updates, 0 creates, 0 deletes; no changed fields except upstream_url.

Reviewed plan hash: `
e072fffdf1d36d7f7bcf768da861487b9088818d918d06654b7718cce2521b5c
`.

| API | Changed field |
| --- | --- |
| Debug API 022465e0-8a60-4fec-bfe8-e21f026c6e77 | upstream_url |
| Debug API 2529b2b9-dfec-4ab2-910b-0f9bfaf0a4aa | upstream_url |
| Declarative GitOps API | upstream_url |
| Experimental Canary API-0d4479a6 | upstream_url |
| Experimental Canary API-34402453 | upstream_url |
| Experimental Canary API-4e29bf5c | upstream_url |
| Experimental Canary API-5d9d78e2 | upstream_url |
| Experimental Canary API-78086a2f | upstream_url |
| Experimental Canary API-83d13326 | upstream_url |
| Experimental Canary API-d618b125 | upstream_url |
| Experimental v2 Canary API | upstream_url |
| demo | upstream_url |
| impact-guidance-406532 | upstream_url |
| impact-guidance-71cfd3 | upstream_url |
| impact-guidance-84b72a | upstream_url |
| impact-guidance-a0ed7e | upstream_url |
| impact-guidance-a277ba | upstream_url |
| impact-guidance-d0a321 | upstream_url |
| impact-guidance-f6b326 | upstream_url |
| orders | upstream_url |
| parity-api-dad395 | upstream_url |
| parity-api-ea0f93 | upstream_url |
| parity-api-ffd34d | upstream_url |
| quota-test-00a7b6 | upstream_url |
| quota-test-4e43d4 | upstream_url |
| quota-test-68a393 | upstream_url |
| quota-test-766b4b | upstream_url |
| rollback-test-api-13041d | upstream_url |
| rollback-test-api-39d3ed | upstream_url |
| rollback-test-api-40d1b6 | upstream_url |
| rollback-test-api-4d87ce | upstream_url |
| rollback-test-api-5390bf | upstream_url |
| rollback-test-api-5c5aa6 | upstream_url |
| rollback-test-api-7420ef | upstream_url |
| rollback-test-api-968fad | upstream_url |
| rollback-test-api-e0fd51 | upstream_url |
| rollback-test-api-f7e9be | upstream_url |
| rollback-test-api-f86450 | upstream_url |
| slow | upstream_url |

Automatic approval review initially required explicit permission for production upstream rewrites. The user then approved all 39 changes. The fresh plan was checked for exact host substitution, no other changed fields and no creates/deletes, then applied with its reviewed hash. Seven other local-only upstreams still require real cloud backend addresses.
