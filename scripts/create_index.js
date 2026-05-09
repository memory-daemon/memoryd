// Ensure the collection exists before creating a search index — Atlas-local
// silently no-ops createSearchIndex() against a non-existent collection,
// leaving callers thinking they're done when they aren't.
if (!db.getCollectionNames().includes("memories")) {
  db.createCollection("memories");
  print("COLLECTION CREATED");
}

// Skip index creation if a vector_index already exists (idempotent re-runs).
const existing = db.memories.getSearchIndexes().find(i => i.name === "vector_index");
if (existing) {
  print("INDEX EXISTS");
  printjson(existing);
} else {
  db.memories.createSearchIndex(
    "vector_index",
    "vectorSearch",
    {
      fields: [
        {
          type: "vector",
          numDimensions: 1024,
          path: "embedding",
          similarity: "cosine"
        }
      ]
    }
  );
  print("INDEX CREATED");
  printjson(db.memories.getSearchIndexes());
}
