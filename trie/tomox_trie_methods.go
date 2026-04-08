package trie

import (
	"errors"
)

// TryGetBestLeftKeyAndValue returns the leftmost leaf in the trie.
// Returns (nil, nil, errors.New("not found")) when the trie is empty.
// Propagates any underlying database error (e.g. MissingNodeError) so the
// caller can distinguish a genuinely empty trie from a persistence failure.
func (t *Trie) TryGetBestLeftKeyAndValue() ([]byte, []byte, error) {
	it := t.NodeIterator(nil)
	for it.Next(true) {
		if it.Leaf() {
			return it.LeafKey(), it.LeafBlob(), nil
		}
	}
	if err := it.Error(); err != nil {
		return nil, nil, err
	}
	return nil, nil, errors.New("not found")
}

// TryGetBestRightKeyAndValue returns the rightmost leaf in the trie (largest key).
// Requires full traversal; slower than the left-key variant.
// Propagates any underlying database error encountered during iteration.
func (t *Trie) TryGetBestRightKeyAndValue() ([]byte, []byte, error) {
	it := t.NodeIterator(nil)
	var lastKey, lastVal []byte
	for it.Next(true) {
		if it.Leaf() {
			lastKey = it.LeafKey()
			lastVal = it.LeafBlob()
		}
	}
	if err := it.Error(); err != nil {
		return nil, nil, err
	}
	if lastKey == nil {
		return nil, nil, errors.New("not found")
	}
	return lastKey, lastVal, nil
}

// TryGetAllLeftKeyAndValue returns up to maxCount leftmost leaves.
// Propagates any underlying database error encountered during iteration.
func (t *Trie) TryGetAllLeftKeyAndValue(maxCount int) ([][]byte, [][]byte, error) {
	it := t.NodeIterator(nil)
	var keys [][]byte
	var vals [][]byte
	count := 0
	for it.Next(true) {
		if it.Leaf() {
			keys = append(keys, it.LeafKey())
			vals = append(vals, it.LeafBlob())
			count++
			if count >= maxCount {
				break
			}
		}
	}
	if err := it.Error(); err != nil {
		return nil, nil, err
	}
	if len(keys) == 0 {
		return nil, nil, errors.New("not found")
	}
	return keys, vals, nil
}
