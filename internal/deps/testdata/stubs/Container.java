// Signatures read from lib-1.0.jar by Java Navigation; no sources are attached.
package com.example.lib;

import java.io.Serializable;
import java.util.AbstractList;
import java.util.Collection;
import java.util.List;
import java.util.Map;

public class Container<E> extends AbstractList<E> implements Serializable {
    public static final int LIMIT = 10;
    public static final String NAME = "box\n";
    public static final long BIG = 1099511627776L;
    protected Map<String, List<E>> index;
    public Container() { /* compiled code */ }
    public Container(int capacity) { /* compiled code */ }
    public E get(int i) { /* compiled code */ }
    public int size() { /* compiled code */ }
    public <K extends Comparable<? super K>> K max(Collection<? extends K> all) { /* compiled code */ }
    public Map.Entry<String, E> entry(long id, double weight) { /* compiled code */ }
    public static class Builder {
        public Builder() { /* compiled code */ }
        public Container.Builder add(Object o) { /* compiled code */ }
        public Container<Object> build() { /* compiled code */ }
    }
    public class Cursor {
        public Cursor(int start) { /* compiled code */ }
        public E next() { /* compiled code */ }
    }
}
