# Database Pagination: Offset-Based vs Keyset (Cursor-Based) Pagination

When designing APIs and database queries that return collections of records, you must paginate the results to prevent overloading server memory, network bandwidth, and the database engine.

There are two primary paradigms for pagination in relational databases (PostgreSQL, MySQL, etc.):
1. **Offset-Based Pagination** (`LIMIT ... OFFSET ...`)
2. **Keyset / Cursor-Based Pagination** (`WHERE key > ... ORDER BY key LIMIT ...`, also known as **Seek Pagination**)

This document breaks down how both work, why offset pagination fails at scale and during concurrent mutations, how keyset pagination solves these problems, and how to implement it cleanly in production.

---

## 1. Offset-Based Pagination

### The Syntax & Mental Model

Offset pagination maps directly to the human concept of pages: *"Skip $N$ items, then give me the next $K$ items."*

```sql
-- Page 1: First 6 items (Page 1, offset 0)
SELECT * FROM listings 
ORDER BY id ASC
LIMIT 6 
OFFSET 0;

-- Page 2: Skip 6 items, take next 6 items (Page 2)
SELECT * FROM listings 
ORDER BY id ASC
LIMIT 6 
OFFSET 6;

-- Page 3: Skip 12 items, take next 6 items (Page 3)
SELECT * FROM listings 
ORDER BY id ASC
LIMIT 6 
OFFSET 12;
```

Formula for offset:
$$\text{OFFSET} = (\text{page} - 1) \times \text{limit}$$

### When Offset-Based Pagination is Fine
* **Small or bounded datasets:** When the table has at most a few thousand rows.
* **Infrequently updated data:** Static catalogs, archived read-only audit tables.
* **Direct Page-Jumping Requirements:** Admin UI tables where users must be able to click *"Go to page 47"* directly.

---

## 2. Problems with Offset-Based Pagination

Despite its intuitive API, offset-based pagination has three severe structural flaws.

---

### Problem 1: Deep Offset Scaling Bottleneck ($O(N)$ Scanning Cost)

What happens when a client requests page 83,334?
```sql
SELECT * FROM listings 
ORDER BY id ASC
LIMIT 6 
OFFSET 500000;
```

#### The Internal Database Reality
A relational database table is not an array stored contiguously in RAM. You cannot perform $O(1)$ random access pointer arithmetic like `array[500000]`.

PostgreSQL must:
1. Scan and read **500,000 rows** from disk/shared buffers.
2. Check MVCC visibility for each row.
3. Sort them if ordering is required.
4. Count through 500,000 rows in memory.
5. **Discard all 500,000 rows.**
6. Retain and return only the final 6 rows.

The official PostgreSQL documentation states this explicitly:
> *"The rows skipped by an `OFFSET` clause still have to be computed inside the server; therefore a large `OFFSET` might be inefficient."*

#### Time Complexity
* **Time to fetch page:** $O(N + K)$, where $N$ is the offset and $K$ is the limit.
* As $N \to \infty$, query latency, buffer hit counts, and disk I/O grow linearly until the database CPU spikes and queries time out.

```
Page 1   (OFFSET 0):       ██ 0.3 ms
Page 100 (OFFSET 600):     ███ 1.2 ms
Page 10k (OFFSET 60,000):  ██████████████ 85 ms
Page 80k (OFFSET 500,000): ████████████████████████████████████████ 1200+ ms
```

---

### Problem 2: Data Drift & Unstable Boundaries (Ghost Rows & Duplicates)

Because `OFFSET` is purely positional, **any insertion or deletion that occurs while a user traverses pages causes the offset boundary to shift**.

#### Scenario A: Record Deletion $\rightarrow$ Missed Record (Ghost Row)

Suppose Page 1 displays records `[1, 2, 3, 4, 5, 6]`.

```
Initial State:
Page 1 (OFFSET 0, LIMIT 6): [1,  2,  3,  4,  5,  6]
Page 2 (OFFSET 6, LIMIT 6): [7,  8,  9, 10, 11, 12]

Event: While the user is viewing Page 1, record '3' is DELETED.

New Table State:
Index:   0   1   2   3   4   5  |  6   7   8   9  10  11
Record: [1,  2,  4,  5,  6,  7] | [8,  9, 10, 11, 12, 13]
                                ▲
                          OFFSET 6 starts here!
```

1. Record `3` is deleted.
2. Record `7` shifts from index 6 to index 5 (it is now in Page 1's position).
3. The user clicks **Next** $\rightarrow$ API executes `OFFSET 6 LIMIT 6`.
4. The database skips the first 6 records (`[1, 2, 4, 5, 6, 7]`) and returns `[8, 9, 10, 11, 12, 13]`.
5. **Result: Record `7` was never displayed to the user. It vanished silently.**

---

#### Scenario B: Record Insertion $\rightarrow$ Duplicate Record

Suppose Page 1 displays records `[1, 2, 3, 4, 5, 6]`.

```
Initial State:
Page 1 (OFFSET 0, LIMIT 6): [1,  2,  3,  4,  5,  6]
Page 2 (OFFSET 6, LIMIT 6): [7,  8,  9, 10, 11, 12]

Event: A new record '0' is inserted at the start of the table.

New Table State:
Index:   0   1   2   3   4   5  |  6   7   8   9  10  11
Record: [0,  1,  2,  3,  4,  5] | [6,  7,  8,  9, 10, 11]
                                ▲
                          OFFSET 6 starts here!
```

1. Record `0` is inserted at the top.
2. Every existing record shifts right by 1 position. Record `6` moves to index 6.
3. The user clicks **Next** $\rightarrow$ API executes `OFFSET 6 LIMIT 6`.
4. The database skips the first 6 records (`[0, 1, 2, 3, 4, 5]`) and returns `[6, 7, 8, 9, 10, 11]`.
5. **Result: Record `6` was already displayed on Page 1, and now appears again on Page 2 (Duplicate record).**

In high-write environments (e.g., social feeds, e-commerce listings, chat feeds), users see duplicates and missing items constantly.

---

### Problem 3: Non-Deterministic Ordering Without `ORDER BY`

SQL relational theory defines tables as **unordered mathematical sets**.

If you run:
```sql
-- DANGEROUS: No ORDER BY
SELECT * FROM listings 
LIMIT 6 
OFFSET 0;
```

* PostgreSQL returns rows in whichever physical order the storage manager encounters them (heap scan order).
* This order can change between queries due to:
  * Concurrent updates (MVCC creates new versions at different physical disk blocks).
  * VACUUM cleaning and reusing dead tuples.
  * Parallel query workers reading pages in different sequences.
* **Without a deterministic `ORDER BY`, pagination is completely broken** because Page 1 and Page 2 might return overlapping sets of rows even if no data changed.
* Furthermore, adding `ORDER BY` with high `OFFSET` forces the engine to sort all $N + K$ rows before discarding, making Problem 1 even slower.

---

## 3. Keyset Pagination (Cursor-Based / Seek Pagination)

### The Mental Model

Instead of saying:
> *"Skip the first 500,000 rows and give me 6"* (Offset)

Keyset pagination says:
> *"Give me the first 6 rows whose key is greater than the last key I saw"* (Seek)

You use the values of the ordered columns from the **last item of the previous page** as a filter predicate (`WHERE id > $last_seen_id`).

```
                    B-Tree Index on 'id'
                           [10]
                         /      \
                      [4]        [16]
                     /   \       /   \
                   [1-3] [5-9] [11-15] [17-20]
                            ▲
                            │ Fast O(log N) B-Tree Seek
                     "WHERE id > 6"
```

---

### How It Works in Practice

#### Step 1: Client Requests Page 1
The client requests the initial page without any cursor.

```sql
SELECT id, title, price, created_at 
FROM listings 
ORDER BY id ASC 
LIMIT 6;
```

**Returned Data:**
```
+----+-------------+--------+
| id | title       | price  |
+----+-------------+--------+
|  1 | MacBook Pro | 1200   |
|  2 | iPhone 13   |  600   |
|  3 | iPad Air    |  450   |
|  4 | Magic Mouse |   70   |
|  5 | Keychron K2 |   80   |
|  6 | Dell 4K Mon |  350   |  <-- Last record has id = 6
+----+-------------+--------+
```

The API response returns the data along with a pagination token/cursor:
`after_key = 6` (or `next_cursor = "6"`).

---

#### Step 2: Client Requests Page 2 (Passing `after_key = 6`)
To get the next page, the client sends `?after_key=6`. The server runs:

```sql
SELECT id, title, price, created_at 
FROM listings 
WHERE id > 6               -- Seek predicate
ORDER BY id ASC 
LIMIT 6;
```

**Returned Data:**
```
+----+-------------+--------+
| id | title       | price  |
+----+-------------+--------+
|  7 | Sony WH1000 |  250   |
|  8 | Bose QC45   |  220   |
|  9 | Studio Mic  |  150   |
| 10 | Ring Light  |   40   |
| 11 | Desk Mat    |   25   |
| 12 | USB-C Hub   |   50   |  <-- Last record has id = 12
+----+-------------+--------+
```

The API response returns `after_key = 12`.

---

#### Step 3: Client Requests Page 3 (Passing `after_key = 12`)
```sql
SELECT id, title, price, created_at 
FROM listings 
WHERE id > 12
ORDER BY id ASC 
LIMIT 6;
```

---

## 4. Why Keyset Pagination Solves Every Offset Problem

| Problem in Offset | How Keyset Solves It |
| :--- | :--- |
| **Deep Offset $O(N)$ Scan** | **$O(\log N)$ or $O(1)$ B-Tree Seek:** The database traverses the B-Tree index directly to `id = 6` in logarithmic time, reads only the next 6 index pointers, and fetches those exact 6 heap rows. Page 1,000,000 executes in the exact same fraction of a millisecond as Page 1. |
| **Record Deletion (Skipped Row)** | If record `3` is deleted, the query `WHERE id > 6` is unaffected. It still immediately seeks to the first key greater than 6. No rows are skipped. |
| **Record Insertion (Duplicate Row)** | If record `0` is inserted at the top, the query `WHERE id > 6` ignores it entirely because $0 \ngtr 6$. No duplicate records appear. |
| **Non-Deterministic Order** | The sort order is strictly anchored to an indexed, deterministic key. |

---

## 5. Real-World Scenario: UUID (v4) + `created_at DESC` (Row Constructor Comparison)

In modern architectures, primary keys are frequently **UUIDv4** instead of auto-incrementing integers (`BIGSERIAL`). Additionally, users almost always want to see **newly created items first** (`ORDER BY created_at DESC`).

This introduces two distinct engineering challenges:
1. **UUIDv4 is completely random:** It has no chronological sequence, so you cannot do `ORDER BY id DESC` to get recent items.
2. **`created_at` is non-unique:** Multiple records can be created within the exact same millisecond or second.

---

### The Problem: Naive Pagination with `created_at` Alone Fails

Suppose you paginate solely using `created_at`:

```sql
-- DANGEROUS: Pagination with created_at alone
SELECT * FROM listings 
WHERE created_at < $1 
ORDER BY created_at DESC
LIMIT 6;
```

#### Why it Breaks: Concurrency Timestamp Collisions
Under real traffic (or batch inserts), multiple records will share the **exact same timestamp**:

```
Record 1:  10:05:00  UUID-D
Record 2:  10:05:00  UUID-C
Record 3:  10:05:00  UUID-B
Record 4:  10:05:00  UUID-A
```

Suppose `LIMIT = 2`:
* **Page 1** fetches the first 2 records: `(10:05:00, UUID-D)` and `(10:05:00, UUID-C)`.
* For **Page 2**, the client sends the last seen timestamp `$1 = '10:05:00'`.
* The server runs: `WHERE created_at < '10:05:00'`.

**The Failure:**
* Both `UUID-B` and `UUID-A` have `created_at = '10:05:00'`.
* Since `'10:05:00' < '10:05:00'` evaluates to **FALSE**, both records are **completely skipped and lost forever from the UI!**
* If you change `<` to `<=`, then `UUID-D` and `UUID-C` are fetched again as **duplicate records**.

---

### The Solution: `created_at + UUID` as Position (The Deterministic Tie-Breaker)

To fix this, we combine `created_at` with the `id` (UUID). 

While UUIDv4 is not chronological, it has two critical properties:
1. **It is globally unique** $\rightarrow$ No two records will ever collide.
2. **It is lexicographically comparable** $\rightarrow$ PostgreSQL can compare UUIDs using `<` and `>` (e.g., `'a...' < 'b...'`).

We use the UUID as a **tie-breaker** to differentiate records that share the same timestamp.

---

### The Query: Row Constructor Comparison

In SQL, this is written cleanly using a **Row Constructor Comparison** (tuple comparison):

```sql
SELECT * FROM listings 
WHERE (created_at, id) < ($1, $2)
ORDER BY created_at DESC, id DESC
LIMIT 6;
```

Here:
* `$1` = `created_at` of the last item on the current page.
* `$2` = `id` (UUID) of the last item on the current page.

---

### Deep Dive: How Row Constructor Comparison Works

The clause `WHERE (created_at, id) < ($1, $2)` is standardized SQL syntax. 

Internally, PostgreSQL evaluates it lexicographically (just like comparing words in a dictionary):

$$\text{WHERE } \text{created\_at} < \$1 \;\;\mathbf{OR}\;\; (\text{created\_at} = \$1 \;\mathbf{AND}\; \text{id} < \$2)$$

#### Step-by-Step Execution Walkthrough:

Take our collision example with `LIMIT = 2`:
```
Table rows (sorted by created_at DESC, id DESC):
1. 10:05:00  UUID-D
2. 10:05:00  UUID-C
3. 10:05:00  UUID-B
4. 10:05:00  UUID-A
5. 10:04:59  UUID-Z
```

#### Page 1:
```sql
SELECT * FROM listings 
ORDER BY created_at DESC, id DESC
LIMIT 2;
```
* **Returns:** `(10:05:00, UUID-D)` and `(10:05:00, UUID-C)`.
* **Last row cursor:** `$1 = '10:05:00'`, `$2 = 'UUID-C'`.

#### Page 2:
```sql
SELECT * FROM listings 
WHERE (created_at, id) < ('10:05:00', 'UUID-C')
ORDER BY created_at DESC, id DESC
LIMIT 2;
```
* PostgreSQL evaluates row 3 `(10:05:00, UUID-B)`:
  * Is `10:05:00 < 10:05:00`? False.
  * Is `10:05:00 = 10:05:00` **AND** `UUID-B < UUID-C`? **TRUE!** $\rightarrow$ Row 3 is included!
* PostgreSQL evaluates row 4 `(10:05:00, UUID-A)`:
  * `10:05:00 = 10:05:00` **AND** `UUID-A < UUID-C` $\rightarrow$ **TRUE!** $\rightarrow$ Row 4 is included!
* **Returns:** `(10:05:00, UUID-B)` and `(10:05:00, UUID-A)`.

#### Page 3:
```sql
SELECT * FROM listings 
WHERE (created_at, id) < ('10:05:00', 'UUID-A')
ORDER BY created_at DESC, id DESC
LIMIT 2;
```
* PostgreSQL reaches row 5 `(10:04:59, UUID-Z)`:
  * Is `10:04:59 < 10:05:00`? **TRUE!** $\rightarrow$ Seamlessly moves to older timestamps!

**Result:** Zero missed rows, zero duplicate rows, completely deterministic.

---

### Required Supporting Composite Index

For PostgreSQL to execute this seek without scanning the table, create a matching composite B-Tree index:

```sql
CREATE INDEX idx_listings_created_at_id 
ON listings (created_at DESC, id DESC);
```

> [!IMPORTANT]
> The index column order and sort direction **must match** your query:
> * Query: `ORDER BY created_at DESC, id DESC`
> * Index: `(created_at DESC, id DESC)`
>
> PostgreSQL's query planner can match `(created_at, id) < ($1, $2)` directly against this composite index, performing a single B-tree seek in $O(\log N)$ time and scanning only the 6 rows needed.

---

### Filtering + Pagination: The Partial Composite Index

In practice, pagination almost always includes filters (e.g., only showing active or `ready` listings):

```sql
SELECT * FROM listings 
WHERE status = 'ready' 
  AND (created_at, id) < ('10:05:00', 'UUID-C')
ORDER BY created_at DESC, id DESC
LIMIT 2;
```

#### The Performance Trap with a Standard Index
If you only have `(created_at DESC, id DESC)`:
1. The database seeks to `(created_at, id)` in the index.
2. For every row, it must fetch the full heap tuple from disk/RAM to check if `status = 'ready'`.
3. If listings have statuses like `draft`, `sold`, `archived`, or `processing`, the engine must discard non-matching rows and continue scanning the index until it collects 2 `ready` listings.
4. **This degrades query latency dramatically** (Index Scan with massive Filter discarding).

#### The Solution: Partial Index (Filtered Composite Index)
In PostgreSQL, create a **Partial Index** that only indexes rows satisfying `status = 'ready'`:

```sql
CREATE INDEX listings_ready_pagination_idx 
ON listings (created_at DESC, id DESC) 
WHERE status = 'ready';
```

#### Why Partial Indexes are Superior:
* **Zero Heap Filtering Overhead:** Every single entry in the B-Tree is guaranteed to be `status = 'ready'`. PostgreSQL performs an instantaneous $O(\log N)$ seek to `('10:05:00', 'UUID-C')` and immediately reads the next 2 rows without checking heap visibility filters.
* **Radically Smaller Footprint:** The index does not store `draft`, `sold`, or `archived` rows, keeping the B-Tree compact in memory (`shared_buffers`).

---

## 6. API Design & Best Practices

### 1. Transparent vs Opaque Cursors

* **Transparent Key (`after_key=6`):**
  * Simple and readable for internal tools or strictly ID-based pagination.
  * *Downside:* Couples the client to your internal database column names and schema.
* **Opaque Base64 Cursor (Recommended for Production):**
  * Clients treat the cursor as a black-box token and pass it via query parameters.

#### The Production Cursor Payload (with Versioning)
Always include a `version` field in the cursor payload. This allows your backend to evolve its pagination logic, change sort algorithms, or modify cursor schemas without breaking existing clients or mobile apps with cached cursors:

```json
{
  "id": "e4b2d589-9832-47d3-b184-eef7b539bf57",
  "created_at": "2026-03-20T10:05:00Z",
  "version": 1
}
```

#### Step 1: Base64 Encode on the Server
Serialize the JSON payload and Base64-encode it:
```text
eyJpZCI6ImU0YjJkNTg5LTk4MzItNDdkMy1iMTg0LWVlZjdiNTM5YmY1NyIsImNyZWF0ZWRfYXQiOiIyMDI2LTAzLTIwVDEwOjA1OjAwWiIsInZlcnNpb24iOjF9
```

#### Step 2: Client Request
The client sends the cursor as an opaque query parameter:
```http
GET /listings?limit=6&after=eyJpZCI6ImU0YjJkNTg5LTk4MzItNDdkMy1iMTg0LWVlZjdiNTM5YmY1NyIsImNyZWF0ZWRfYXQiOiIyMDI2LTAzLTIwVDEwOjA1OjAwWiIsInZlcnNpb24iOjF9
```

#### Step 3: Server Decodes & Seeks
1. Decode the Base64 token into JSON.
2. Validate `version == 1`.
3. Extract `created_at` $\rightarrow$ `$1`, `id` $\rightarrow$ `$2`.
4. Execute:
   ```sql
   SELECT * FROM listings 
   WHERE status = 'ready' 
     AND (created_at, id) < ($1, $2)
   ORDER BY created_at DESC, id DESC
   LIMIT 7; -- (limit + 1 for has_more check)
   ```

---

### 2. Efficiently Detecting `has_more` (The `LIMIT + 1` Trick)

Never run `SELECT COUNT(*)` to find out if another page exists—`COUNT(*)` in PostgreSQL forces a full table scan and negates pagination performance.

Instead, **fetch `LIMIT + 1` rows**:

```sql
-- Client requests limit = 6, backend queries limit = 7
SELECT id, title, price, status, created_at 
FROM listings 
WHERE status = 'ready' 
  AND (created_at, id) < ($1, $2)
ORDER BY created_at DESC, id DESC 
LIMIT 7;
```

* **If database returns 7 rows:**
  * You know there is a next page (`has_more = true`).
  * Discard the 7th row from the response array.
  * Use the 6th row (`id`, `created_at`) to generate the Base64 `next_cursor` (with `"version": 1`).
  * Return exactly 6 rows to the client.
* **If database returns $\le 6$ rows:**
  * You are on the last page (`has_more = false`).
  * Set `next_cursor = null`.

### Standard API Response Envelope

```json
{
  "data": [
    {
      "id": "e4b2d589-9832-47d3-b184-eef7b539bf57",
      "title": "Sony WH1000",
      "price": 250,
      "status": "ready",
      "created_at": "2026-03-20T10:05:00Z"
    },
    {
      "id": "7ac1d234-5821-4d11-8231-abf82910cd12",
      "title": "Bose QC45",
      "price": 220,
      "status": "ready",
      "created_at": "2026-03-20T10:05:00Z"
    }
  ],
  "pagination": {
    "limit": 6,
    "next_cursor": "eyJpZCI6IjdhYzFkMjM0LTU4MjEtNGQxMS04MjMxLWFiZjgyOTEwY2QxMiIsImNyZWF0ZWRfYXQiOiIyMDI2LTAzLTIwVDEwOjA1OjAwWiIsInZlcnNpb24iOjF9",
    "has_more": true
  }
}
```

---

## 7. Offset vs Keyset: Quick Comparison Matrix

| Feature | Offset Pagination (`OFFSET N`) | Keyset / Cursor Pagination (`WHERE key > K`) |
| :--- | :--- | :--- |
| **SQL Complexity** | Simple (`OFFSET 12 LIMIT 6`) | Moderate (`WHERE (col, id) > (...)`) |
| **Deep Page Performance** | $O(N)$ — Becomes unusable on large tables | $O(\log N)$ or $O(1)$ — Uniformly fast |
| **Data Drift Resilience** | Fragile (misses or duplicates rows on write) | Immune (anchored to deterministic key) |
| **Jump to Arbitrary Page** | Yes (`page=50`) | No (requires traversing sequential cursors) |
| **Total Count Required** | Usually paired with slow `COUNT(*)` | Not needed; uses `LIMIT + 1` trick |
| **Best Use Cases** | Admin dashboards, internal tools with $< 10\text{k}$ rows | Infinite feeds, public APIs, high-write tables, high-scale services |

---

## Summary Checklist for Production

1. Default to **Keyset / Cursor Pagination** for any public-facing API, infinite scroll, or high-volume dataset.
2. Ensure every keyset query has a **matching B-Tree index** on the exact columns in the `ORDER BY` and `WHERE` clauses.
3. When pagination is coupled with a fixed status filter (e.g. `WHERE status = 'ready'`), use a **Partial Composite Index** (`... WHERE status = 'ready'`) to eliminate heap filter scanning and keep index memory tiny.
4. Always include a unique column (like `id` UUIDv4 or `uuid_v7`) as a **tie-breaker** when sorting by non-unique fields like `created_at` or `price`, using **row constructor comparisons**: `WHERE (created_at, id) < ($1, $2)`.
5. Use the **`LIMIT + 1` pattern** to compute `has_more` and avoid expensive `COUNT(*)` queries.
6. Use **opaque, versioned Base64 tokens** for cursors (e.g., `{"id": "...", "created_at": "...", "version": 1}`) to allow safe schema evolution without breaking clients.
