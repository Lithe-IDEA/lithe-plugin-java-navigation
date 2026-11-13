package com.example.lib;

import java.io.Serializable;
import java.util.AbstractList;
import java.util.Collection;
import java.util.List;
import java.util.Map;

public class Container<E> extends AbstractList<E> implements Serializable {
    public static final int LIMIT = 10;
    public static final String NAME = "box\n";
    public static final long BIG = 1L << 40;
    protected Map<String, List<E>> index;
    private int hidden;

    public Container() {
    }

    public Container(int capacity) {
    }

    @Override
    public E get(int i) {
        return null;
    }

    @Override
    public int size() {
        return hidden;
    }

    public <K extends Comparable<? super K>> K max(Collection<? extends K> all) {
        return null;
    }

    public Map.Entry<String, E> entry(long id, double weight) {
        return null;
    }

    private void secret() {
    }

    public class Cursor {
        public Cursor(int start) {
        }

        public E next() {
            return null;
        }
    }

    public static class Builder {
        public Builder add(Object o) {
            return this;
        }

        public Container<Object> build() {
            return new Container<>();
        }
    }
}
